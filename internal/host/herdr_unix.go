//go:build unix

package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Herdr runs each worker in its own new herdr tab in the operator's workspace (HO3). The
// tab only shows the worker: stdout, stderr, the pid and the exit code all go through files
// written by a wrapper script, so results never depend on the tab.
type Herdr struct {
	Bin          string        // herdr binary; "herdr" resolves on PATH
	Workspace    string        // herdr workspace for worker tabs (HERDR_WORKSPACE_ID); "" = herdr's default
	Poll         time.Duration // how often pid and exit files are polled
	StartTimeout time.Duration // how long Start waits for the pid file
	Grace        time.Duration // how long Wait waits for the exit file once the wrapper is gone
	// Pgrun is the argv prefix that runs a command as the leader of a new process group, in
	// place; empty = the running mill binary with `__pgrun --`.
	Pgrun []string
}

// NewHerdr returns the herdr host using the herdr binary on PATH and the workspace in
// HERDR_WORKSPACE_ID.
func NewHerdr() *Herdr {
	return &Herdr{Bin: "herdr", Workspace: os.Getenv("HERDR_WORKSPACE_ID"), Poll: 50 * time.Millisecond, StartTimeout: 30 * time.Second, Grace: 2 * time.Second}
}

// Name implements Host.
func (*Herdr) Name() string { return "herdr" }

func (h *Herdr) grace() time.Duration {
	if h.Grace <= 0 {
		return 2 * time.Second
	}
	return h.Grace
}

func (h *Herdr) poll() time.Duration {
	if h.Poll <= 0 {
		return 50 * time.Millisecond
	}
	return h.Poll
}

func (h *Herdr) startTimeout() time.Duration {
	if h.StartTimeout <= 0 {
		return 30 * time.Second
	}
	return h.StartTimeout
}

// pgrun returns the argv prefix that starts the worker in a new process group.
func (h *Herdr) pgrun() ([]string, error) {
	if len(h.Pgrun) > 0 {
		return h.Pgrun, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("herdr host: find the mill binary to start the worker's process group with `mill __pgrun`: %w; set host.kind = \"exec\" to run workers without herdr", err)
	}
	return []string{exe, "__pgrun", "--"}, nil
}

// herdr runs the herdr binary and returns its stdout. A failure includes herdr's stderr.
func (h *Herdr) herdr(args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(h.Bin, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("herdr host: %s %s: %w: %s", h.Bin, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// Start implements Host.
func (h *Herdr) Start(ctx context.Context, spec ProcSpec) (Handle, error) {
	if len(spec.Argv) == 0 {
		return nil, errors.New("herdr host: empty argv")
	}
	if spec.StdinPath != "" {
		if _, err := os.Stat(spec.StdinPath); err != nil {
			return nil, fmt.Errorf("herdr host: stdin: %w", err)
		}
	}
	for _, p := range []string{spec.StdoutPath, spec.StderrPath} {
		if p == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, fmt.Errorf("herdr host: %w", err)
		}
	}
	pgrun, err := h.pgrun()
	if err != nil {
		return nil, err
	}
	files, err := writeWrapper(spec, pgrun)
	if err != nil {
		return nil, err
	}

	dir := spec.Dir
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return nil, fmt.Errorf("herdr host: working directory: %w", err)
		}
	}
	// Each worker gets its own tab, never --focus: the operator's focus stays where it is.
	args := []string{"tab", "create"}
	if h.Workspace != "" {
		args = append(args, "--workspace", h.Workspace)
	}
	args = append(args, "--label", "mill "+workerName(spec), "--cwd", dir)
	out, err := h.herdr(args...)
	if err != nil {
		return nil, err
	}
	pane, tab, err := parseTabCreate(out)
	if err != nil {
		return nil, err
	}
	hd := &herdrHandle{host: h, ctx: ctx, pane: pane, tab: tab, files: files, done: make(chan struct{}), watched: make(chan struct{})}
	// The tab file lets crash recovery close a tab a killed mill left open; the mill pid
	// tells recovery whether the mill that started the worker is still running.
	if err := writeTabFile(files.tab, HerdrTab{Tab: tab, Workspace: h.Workspace, MillPID: os.Getpid()}); err != nil {
		return nil, errors.Join(err, hd.closeTab())
	}

	if _, err := h.herdr("pane", "run", hd.pane, "sh", files.wrapper); err != nil {
		return nil, errors.Join(err, hd.closeTab())
	}
	pid, wrapperPID, err := h.waitPID(files.pid)
	if err != nil {
		return nil, errors.Join(err, hd.closeTab())
	}
	hd.pid, hd.wrapperPID = pid, wrapperPID

	go func() {
		defer close(hd.watched)
		select {
		case <-ctx.Done():
			// Wait returns this error alongside the exit.
			err := hd.Kill()
			hd.mu.Lock()
			hd.killErr = err
			hd.mu.Unlock()
		case <-hd.done:
		}
	}()
	return hd, nil
}

// workerName is the name shown in the tab label and the notice line.
func workerName(spec ProcSpec) string {
	name := strings.TrimSpace(strings.Join(strings.Fields(spec.Name), " "))
	if name == "" {
		return "worker"
	}
	return name
}

// parseTabCreate returns the root pane id and the tab id from herdr's tab create response.
func parseTabCreate(out []byte) (pane, tab string, err error) {
	var resp struct {
		Result struct {
			RootPane struct {
				PaneID string `json:"pane_id"`
			} `json:"root_pane"`
			Tab struct {
				TabID string `json:"tab_id"`
			} `json:"tab"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", "", fmt.Errorf("herdr host: tab create returned %q, not JSON with .result.root_pane.pane_id: check the herdr version", strings.TrimSpace(string(out)))
	}
	if resp.Result.RootPane.PaneID == "" {
		return "", "", fmt.Errorf("herdr host: tab create returned no .result.root_pane.pane_id in %q: check the herdr version", strings.TrimSpace(string(out)))
	}
	if resp.Result.Tab.TabID == "" {
		return "", "", fmt.Errorf("herdr host: tab create returned no .result.tab.tab_id in %q: check the herdr version", strings.TrimSpace(string(out)))
	}
	return resp.Result.RootPane.PaneID, resp.Result.Tab.TabID, nil
}

// HerdrTab is a worker tab recorded in a herdr-*.tab file next to the wrapper's files.
type HerdrTab struct {
	Tab       string `json:"tab"`
	Workspace string `json:"workspace"` // "" = herdr's default workspace
	// MillPID is the pid of the mill process that started the worker; 0 = not recorded
	// (tab files written before mill recorded it).
	MillPID int `json:"mill_pid,omitempty"`
	// Bin is the herdr binary CloseHerdrTab runs; "" resolves "herdr" on PATH. It is not
	// recorded: the caller sets it the way it selects the herdr host.
	Bin string `json:"-"`
	// File is the herdr-*.tab file the tab was read from.
	File string `json:"-"`
}

// writeTabFile records t in path as JSON {"tab": ..., "workspace": ..., "mill_pid": ...}.
func writeTabFile(path string, t HerdrTab) error {
	b, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("herdr host: tab file %s: %w", path, err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("herdr host: write tab file %s: %w; the worker's tab was closed", path, err)
	}
	return nil
}

// RecordedHerdrTabs returns the tabs recorded in every herdr-*.tab file in dir, in file name
// order. A missing dir holds none.
func RecordedHerdrTabs(dir string) ([]HerdrTab, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("herdr host: read recorded tabs in %s: %w", dir, err)
	}
	var tabs []HerdrTab
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "herdr-") || !strings.HasSuffix(name, ".tab") {
			continue
		}
		path := filepath.Join(dir, name)
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("herdr host: read tab file %s: %w", path, err)
		}
		var t HerdrTab
		if err := json.Unmarshal(b, &t); err != nil || t.Tab == "" {
			return nil, fmt.Errorf("herdr host: tab file %s holds %q, not {\"tab\": ..., \"workspace\": ...}: close the tab by hand and delete the file", path, strings.TrimSpace(string(b)))
		}
		t.File = path
		tabs = append(tabs, t)
	}
	return tabs, nil
}

// CloseHerdrTab runs `herdr tab close <tab>`. It returns closed=false and no error when herdr
// is not installed or reports that the tab does not exist (it was already closed).
func CloseHerdrTab(t HerdrTab) (closed bool, err error) {
	bin := t.Bin
	if bin == "" {
		if bin, err = exec.LookPath("herdr"); err != nil {
			return false, nil
		}
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, "tab", "close", t.Tab)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		msg := strings.TrimSpace(stderr.String() + " " + stdout.String())
		if tabMissing(msg) {
			return false, nil
		}
		return false, fmt.Errorf("herdr host: %s tab close %s: %w: %s; close herdr tab %s by hand", bin, t.Tab, err, msg, t.Tab)
	}
	return true, nil
}

// tabMissing reports whether herdr's output says the tab does not exist.
func tabMissing(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range []string{"not found", "not_found", "no such", "does not exist", "doesn't exist", "unknown tab"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// waitPID waits for the pid file, which holds the pid of the worker's process group leader
// and the pid of the outer wrapper that writes the exit file.
func (h *Herdr) waitPID(path string) (pid, wrapperPID int, err error) {
	deadline := time.Now().Add(h.startTimeout())
	for {
		if b, err := os.ReadFile(path); err == nil {
			if f := strings.Fields(string(b)); len(f) == 2 {
				pid, err1 := strconv.Atoi(f[0])
				wpid, err2 := strconv.Atoi(f[1])
				if err1 == nil && err2 == nil && pid > 0 && wpid > 0 {
					return pid, wpid, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return 0, 0, fmt.Errorf("herdr host: the wrapper never wrote its pid to %s within %s: check the herdr tab for errors", path, h.startTimeout())
		}
		time.Sleep(h.poll())
	}
}

// wrapperFiles are the files one run uses to talk to mill.
type wrapperFiles struct {
	wrapper, pid, rc, exit, tab string
}

// writeWrapper writes the wrapper script into the log directory. Run as `sh wrapper`, it
// prints a notice naming the worker, then starts itself again as `sh wrapper run <pid>`
// through the pgrun prefix, which makes the inner copy the leader of a new process group
// without job control, so its pid is the group id. That inner copy writes its own pid and
// the outer one, runs the command and records its exit code; the outer copy survives a kill
// of the group and writes the final exit code atomically.
func writeWrapper(spec ProcSpec, pgrun []string) (wrapperFiles, error) {
	dir := spec.LogDir
	if dir == "" && spec.StdoutPath != "" {
		dir = filepath.Dir(spec.StdoutPath)
	}
	if dir == "" {
		d, err := os.MkdirTemp("", "mill-herdr-")
		if err != nil {
			return wrapperFiles{}, fmt.Errorf("herdr host: log dir: %w", err)
		}
		dir = d
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return wrapperFiles{}, fmt.Errorf("herdr host: log dir: %w", err)
	}
	f, err := os.CreateTemp(dir, "herdr-*.sh")
	if err != nil {
		return wrapperFiles{}, fmt.Errorf("herdr host: wrapper: %w", err)
	}
	base := strings.TrimSuffix(f.Name(), ".sh")
	files := wrapperFiles{wrapper: f.Name(), pid: base + ".pid", rc: base + ".rc", exit: base + ".exit", tab: base + ".tab"}

	orNull := func(p string) string {
		if p == "" {
			return "/dev/null"
		}
		return p
	}
	cmd := []string{"env", "-i"}
	for _, kv := range spec.Env {
		cmd = append(cmd, shQuote(kv))
	}
	for _, a := range spec.Argv {
		cmd = append(cmd, shQuote(a))
	}
	cd := ""
	if spec.Dir != "" {
		cd = "cd " + shQuote(spec.Dir) + " && "
	}
	prefix := make([]string, len(pgrun))
	for i, a := range pgrun {
		prefix[i] = shQuote(a)
	}
	w, pid, rc, exit := shQuote(files.wrapper), shQuote(files.pid), shQuote(files.rc), shQuote(files.exit)
	script := "#!/bin/sh\n" +
		"# mill herdr wrapper" + strings.ReplaceAll(" "+spec.Name, "\n", " ") + "\n" +
		"if [ \"$1\" = run ]; then\n" +
		"  printf '%s %s\\n' \"$$\" \"$2\" > " + pid + ".tmp && mv " + pid + ".tmp " + pid + " || exit 1\n" +
		"  { " + cd + strings.Join(cmd, " ") + " < " + shQuote(orNull(spec.StdinPath)) + " 2>> " + shQuote(orNull(spec.StderrPath)) +
		"; printf '%s\\n' \"$?\" > " + rc + ".tmp && mv " + rc + ".tmp " + rc + "; } | tee -a " + shQuote(orNull(spec.StdoutPath)) + "\n" +
		"  exit 0\n" +
		"fi\n" +
		"printf '%s\\n' " + shQuote("mill worker: "+workerName(spec)+". Closing this tab stops the task.") + "\n" +
		strings.Join(prefix, " ") + " sh " + w + " run \"$$\" &\n" +
		"wait \"$!\"\n" +
		"code=$?\n" +
		"if [ -f " + rc + " ]; then code=$(cat " + rc + "); fi\n" +
		"printf '%s\\n' \"$code\" > " + exit + ".tmp && mv " + exit + ".tmp " + exit + "\n"
	if _, err := f.WriteString(script); err != nil {
		f.Close()
		return wrapperFiles{}, fmt.Errorf("herdr host: wrapper: %w", err)
	}
	if err := f.Close(); err != nil {
		return wrapperFiles{}, fmt.Errorf("herdr host: wrapper: %w", err)
	}
	return files, nil
}

// shQuote quotes s as a single POSIX sh word.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

type herdrHandle struct {
	host       *Herdr
	ctx        context.Context
	pane, tab  string
	files      wrapperFiles
	pid        int           // the worker's process group leader
	wrapperPID int           // the outer wrapper, which writes the exit file
	done       chan struct{} // closed when Wait has seen the exit
	// closed when the context watcher has returned
	watched chan struct{}

	mu      sync.Mutex
	killed  bool
	killErr error

	closeOnce sync.Once
	closeErr  error
}

func (h *herdrHandle) PID() int { return h.pid }

// closeTab closes the worker's tab once; later calls return the first result. A closed tab's
// tab file goes with it, so recovery never closes a later tab that reuses its id.
func (h *herdrHandle) closeTab() error {
	h.closeOnce.Do(func() {
		if _, h.closeErr = h.host.herdr("tab", "close", h.tab); h.closeErr != nil {
			return
		}
		if err := os.Remove(h.files.tab); err != nil && !errors.Is(err, os.ErrNotExist) {
			h.closeErr = fmt.Errorf("herdr host: remove tab file of closed tab %s: %w; delete it by hand", h.tab, err)
		}
	})
	return h.closeErr
}

// readExit returns the code from the exit file, or ok=false while it does not exist yet.
func (h *herdrHandle) readExit() (code int, ok bool, err error) {
	b, err := os.ReadFile(h.files.exit)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("herdr host: exit file: %w", err)
	}
	code, err = strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false, fmt.Errorf("herdr host: exit file %s holds %q, not an exit code", h.files.exit, strings.TrimSpace(string(b)))
	}
	return code, true, nil
}

// wrapperGone reports whether the outer wrapper has died: it is gone, or its pid now runs
// another command. A wrapper that died never writes the exit file.
func (h *herdrHandle) wrapperGone() (bool, error) {
	_, command, alive, err := ProcessInfo(h.wrapperPID)
	if err != nil {
		return false, fmt.Errorf("herdr host: check wrapper %d in herdr tab %s: %w", h.wrapperPID, h.tab, err)
	}
	return !alive || !strings.Contains(command, h.files.wrapper), nil
}

// waitExit polls for the exit file. It gives up, with lost=true, when the wrapper has died
// and the file has not appeared within the grace period (the tab was closed or the wrapper
// killed), or when the context was cancelled and the file has not appeared within
// StartTimeout plus the grace period.
func (h *herdrHandle) waitExit() (code int, lost bool, err error) {
	grace := h.host.grace()
	checkEvery := grace / 4
	var lastCheck, goneAt, cancelDeadline time.Time
	for {
		code, ok, err := h.readExit()
		if err != nil || ok {
			return code, false, err
		}
		now := time.Now()
		if cancelDeadline.IsZero() && h.ctx.Err() != nil {
			cancelDeadline = now.Add(h.host.startTimeout() + grace)
		}
		if !cancelDeadline.IsZero() && now.After(cancelDeadline) {
			return -1, true, fmt.Errorf("herdr host: cancelled, but the wrapper in herdr tab %s never wrote %s: the attempt is lost; close the tab by hand if it is still open", h.tab, h.files.exit)
		}
		if goneAt.IsZero() && now.Sub(lastCheck) >= checkEvery {
			lastCheck = now
			gone, err := h.wrapperGone()
			if err != nil {
				return -1, true, err
			}
			if gone {
				goneAt = now
			}
		}
		if !goneAt.IsZero() && now.Sub(goneAt) >= grace {
			return -1, true, fmt.Errorf("herdr host: the herdr pane running the worker (tab %s) was closed or killed before %s was written: the attempt is lost", h.tab, h.files.exit)
		}
		time.Sleep(h.host.poll())
	}
}

func (h *herdrHandle) Wait() (Exit, error) {
	code, lost, err := h.waitExit()
	close(h.done)
	<-h.watched
	if err != nil {
		ex := Exit{Code: -1}
		if lost {
			// Whatever the worker left running goes with it.
			ex.Signaled = true
			err = errors.Join(err, KillGroup(h.pid))
		}
		return ex, errors.Join(err, h.closeTab())
	}
	// Kill anything the process left running in its group (e.g. backgrounded children).
	killErr := KillGroup(h.pid)
	h.mu.Lock()
	killed := h.killed
	killErr = errors.Join(killErr, h.killErr)
	h.mu.Unlock()
	ex := Exit{Code: code, Signaled: code > 128 || killed}
	return ex, errors.Join(killErr, h.closeTab())
}

// Kill stops the process group, waits for the wrapper to record the exit and closes the tab.
func (h *herdrHandle) Kill() error {
	h.mu.Lock()
	h.killed = true
	h.mu.Unlock()
	if err := KillGroup(h.pid); err != nil {
		return fmt.Errorf("herdr host: kill process group %d: %w", h.pid, err)
	}
	deadline := time.Now().Add(h.host.startTimeout())
	for {
		_, ok, err := h.readExit()
		if err != nil {
			return err
		}
		if ok {
			return h.closeTab()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("herdr host: process group %d killed but %s never appeared: close herdr tab %s by hand", h.pid, h.files.exit, h.tab)
		}
		time.Sleep(h.host.poll())
	}
}
