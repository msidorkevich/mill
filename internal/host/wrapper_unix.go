//go:build unix

package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The worker wrapper protocol shared by the terminal hosts (herdr): the worker runs in a
// window of a terminal multiplexer, but its stdout, stderr, pid and exit code all go through
// files written by a wrapper script, so results never depend on the window.

// Defaults of the terminal hosts' polling settings.
const (
	defaultPoll         = 50 * time.Millisecond
	defaultStartTimeout = 30 * time.Second
	defaultGrace        = 2 * time.Second
)

// orDefault returns d, or def when d is unset.
func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// pgrunPrefix returns the argv prefix that starts the worker in a new process group: pgrun,
// or the running mill binary with `__pgrun --` when pgrun is empty. kind names the host in
// errors.
func pgrunPrefix(kind string, pgrun []string) ([]string, error) {
	if len(pgrun) > 0 {
		return pgrun, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("%s host: find the mill binary to start the worker's process group with `mill __pgrun`: %w; set host.kind = \"exec\" to run workers without %s", kind, err, kind)
	}
	return []string{exe, "__pgrun", "--"}, nil
}

// terminal is a terminal multiplexer a host runs each worker in.
type terminal interface {
	// kind names the terminal in errors, e.g. "herdr".
	kind() string
	// open opens a new window labelled label in dir and runs argv in it. record is a file
	// next to the wrapper's files where the terminal may record the window for crash
	// recovery. On failure open leaves no window open.
	open(label, dir string, argv []string, record string) (window, error)
}

// window is a terminal window running one worker.
type window interface {
	// close closes the window; the handle calls it once.
	close() error
	// exists reports whether the window is still open. A terminal that cannot ask reports
	// true: a closed window is then detected through its wrapper dying.
	exists() (bool, error)
	// String describes the window for error messages, e.g. "herdr tab t1".
	String() string
}

// wrapperConfig holds a terminal host's settings, defaults applied.
type wrapperConfig struct {
	poll         time.Duration // how often pid and exit files are polled
	startTimeout time.Duration // how long Start waits for the pid file
	grace        time.Duration // how long Wait waits for the exit file once the wrapper is gone
	pgrun        []string      // argv prefix that starts the worker in a new process group; empty = pgrunPrefix's default
}

// startWrapped writes the wrapper for spec, runs it in a new window of term and waits for
// the worker's pid.
func startWrapped(ctx context.Context, term terminal, cfg wrapperConfig, spec ProcSpec) (*wrapperHandle, error) {
	kind := term.kind()
	if len(spec.Argv) == 0 {
		return nil, fmt.Errorf("%s host: empty argv", kind)
	}
	if spec.StdinPath != "" {
		if _, err := os.Stat(spec.StdinPath); err != nil {
			return nil, fmt.Errorf("%s host: stdin: %w", kind, err)
		}
	}
	for _, p := range []string{spec.StdoutPath, spec.StderrPath} {
		if p == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, fmt.Errorf("%s host: %w", kind, err)
		}
	}
	pgrun, err := pgrunPrefix(kind, cfg.pgrun)
	if err != nil {
		return nil, err
	}
	files, err := writeWrapper(spec, pgrun)
	if err != nil {
		return nil, fmt.Errorf("%s host: %w", kind, err)
	}

	dir := spec.Dir
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return nil, fmt.Errorf("%s host: working directory: %w", kind, err)
		}
	}
	win, err := term.open("mill "+workerName(spec), dir, []string{"sh", files.wrapper}, files.tab)
	if err != nil {
		return nil, err
	}
	hd := &wrapperHandle{kind: kind, cfg: cfg, ctx: ctx, win: win, files: files, done: make(chan struct{}), watched: make(chan struct{})}
	pid, wrapperPID, err := hd.waitPID()
	if err != nil {
		return nil, errors.Join(err, hd.closeWindow())
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

// workerName is the name shown in the window label and the notice line.
func workerName(spec ProcSpec) string {
	name := strings.TrimSpace(strings.Join(strings.Fields(spec.Name), " "))
	if name == "" {
		return "worker"
	}
	return name
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
			return wrapperFiles{}, fmt.Errorf("log dir: %w", err)
		}
		dir = d
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return wrapperFiles{}, fmt.Errorf("log dir: %w", err)
	}
	f, err := os.CreateTemp(dir, "herdr-*.sh")
	if err != nil {
		return wrapperFiles{}, fmt.Errorf("wrapper: %w", err)
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
		return wrapperFiles{}, fmt.Errorf("wrapper: %w", err)
	}
	if err := f.Close(); err != nil {
		return wrapperFiles{}, fmt.Errorf("wrapper: %w", err)
	}
	return files, nil
}

// shQuote quotes s as a single POSIX sh word.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// wrapperHandle is a worker started through the wrapper in a terminal window.
type wrapperHandle struct {
	kind       string // names the host in errors, e.g. "herdr"
	cfg        wrapperConfig
	ctx        context.Context
	win        window
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

func (h *wrapperHandle) PID() int { return h.pid }

// closeWindow closes the worker's window once; later calls return the first result.
func (h *wrapperHandle) closeWindow() error {
	h.closeOnce.Do(func() { h.closeErr = h.win.close() })
	return h.closeErr
}

// waitPID waits for the pid file, which holds the pid of the worker's process group leader
// and the pid of the outer wrapper that writes the exit file.
func (h *wrapperHandle) waitPID() (pid, wrapperPID int, err error) {
	deadline := time.Now().Add(h.cfg.startTimeout)
	for {
		if b, err := os.ReadFile(h.files.pid); err == nil {
			if f := strings.Fields(string(b)); len(f) == 2 {
				pid, err1 := strconv.Atoi(f[0])
				wpid, err2 := strconv.Atoi(f[1])
				if err1 == nil && err2 == nil && pid > 0 && wpid > 0 {
					return pid, wpid, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return 0, 0, fmt.Errorf("%s host: the wrapper never wrote its pid to %s within %s: check %s for errors", h.kind, h.files.pid, h.cfg.startTimeout, h.win)
		}
		time.Sleep(h.cfg.poll)
	}
}

// readExit returns the code from the exit file, or ok=false while it does not exist yet.
func (h *wrapperHandle) readExit() (code int, ok bool, err error) {
	b, err := os.ReadFile(h.files.exit)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("%s host: exit file: %w", h.kind, err)
	}
	code, err = strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false, fmt.Errorf("%s host: exit file %s holds %q, not an exit code", h.kind, h.files.exit, strings.TrimSpace(string(b)))
	}
	return code, true, nil
}

// wrapperGone reports whether the outer wrapper has died: it is gone, or its pid now runs
// another command, or its window was closed. A wrapper that died never writes the exit file.
func (h *wrapperHandle) wrapperGone() (bool, error) {
	_, command, alive, err := ProcessInfo(h.wrapperPID)
	if err != nil {
		return false, fmt.Errorf("%s host: check wrapper %d in %s: %w", h.kind, h.wrapperPID, h.win, err)
	}
	if !alive || !strings.Contains(command, h.files.wrapper) {
		return true, nil
	}
	open, err := h.win.exists()
	if err != nil {
		return false, fmt.Errorf("%s host: check %s: %w", h.kind, h.win, err)
	}
	return !open, nil
}

// waitExit polls for the exit file. It gives up, with lost=true, when the wrapper has died
// and the file has not appeared within the grace period (the window was closed or the
// wrapper killed), or when the context was cancelled and the file has not appeared within
// StartTimeout plus the grace period.
func (h *wrapperHandle) waitExit() (code int, lost bool, err error) {
	grace := h.cfg.grace
	checkEvery := grace / 4
	var lastCheck, goneAt, cancelDeadline time.Time
	for {
		code, ok, err := h.readExit()
		if err != nil || ok {
			return code, false, err
		}
		now := time.Now()
		if cancelDeadline.IsZero() && h.ctx.Err() != nil {
			cancelDeadline = now.Add(h.cfg.startTimeout + grace)
		}
		if !cancelDeadline.IsZero() && now.After(cancelDeadline) {
			return -1, true, fmt.Errorf("%s host: cancelled, but the wrapper in %s never wrote %s: the attempt is lost; close %s by hand if it is still open", h.kind, h.win, h.files.exit, h.win)
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
			return -1, true, fmt.Errorf("%s host: the %s pane running the worker (%s) was closed or killed before %s was written: the attempt is lost", h.kind, h.kind, h.win, h.files.exit)
		}
		time.Sleep(h.cfg.poll)
	}
}

func (h *wrapperHandle) Wait() (Exit, error) {
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
		return ex, errors.Join(err, h.closeWindow())
	}
	// Kill anything the process left running in its group (e.g. backgrounded children).
	killErr := KillGroup(h.pid)
	h.mu.Lock()
	killed := h.killed
	killErr = errors.Join(killErr, h.killErr)
	h.mu.Unlock()
	ex := Exit{Code: code, Signaled: code > 128 || killed}
	return ex, errors.Join(killErr, h.closeWindow())
}

// Kill stops the process group, waits for the wrapper to record the exit and closes the
// window.
func (h *wrapperHandle) Kill() error {
	h.mu.Lock()
	h.killed = true
	h.mu.Unlock()
	if err := KillGroup(h.pid); err != nil {
		return fmt.Errorf("%s host: kill process group %d: %w", h.kind, h.pid, err)
	}
	deadline := time.Now().Add(h.cfg.startTimeout)
	for {
		_, ok, err := h.readExit()
		if err != nil {
			return err
		}
		if ok {
			return h.closeWindow()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s host: process group %d killed but %s never appeared: close %s by hand", h.kind, h.pid, h.files.exit, h.win)
		}
		time.Sleep(h.cfg.poll)
	}
}
