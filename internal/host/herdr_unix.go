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

// Herdr runs each worker in a new herdr pane split off the current one (HO3). The pane only
// shows the worker: stdout, stderr, the pid and the exit code all go through files written
// by a wrapper script, so results never depend on the pane.
type Herdr struct {
	Bin          string        // herdr binary; "herdr" resolves on PATH
	Poll         time.Duration // how often pid and exit files are polled
	StartTimeout time.Duration // how long Start waits for the pid file
}

// NewHerdr returns the herdr host using the herdr binary on PATH.
func NewHerdr() *Herdr {
	return &Herdr{Bin: "herdr", Poll: 50 * time.Millisecond, StartTimeout: 10 * time.Second}
}

// Name implements Host.
func (*Herdr) Name() string { return "herdr" }

func (h *Herdr) poll() time.Duration {
	if h.Poll <= 0 {
		return 50 * time.Millisecond
	}
	return h.Poll
}

func (h *Herdr) startTimeout() time.Duration {
	if h.StartTimeout <= 0 {
		return 10 * time.Second
	}
	return h.StartTimeout
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
	files, err := writeWrapper(spec)
	if err != nil {
		return nil, err
	}

	dir := spec.Dir
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return nil, fmt.Errorf("herdr host: working directory: %w", err)
		}
	}
	out, err := h.herdr("pane", "split", "--current", "--direction", "right", "--no-focus", "--cwd", dir)
	if err != nil {
		return nil, err
	}
	var split struct {
		Result struct {
			Pane json.RawMessage `json:"pane"`
		} `json:"result"`
	}
	pane := ""
	if err := json.Unmarshal(out, &split); err == nil {
		// Real herdr returns an object with pane_id; a plain string id is also accepted.
		var obj struct {
			PaneID string `json:"pane_id"`
		}
		if json.Unmarshal(split.Result.Pane, &pane) != nil {
			pane = ""
			if json.Unmarshal(split.Result.Pane, &obj) == nil {
				pane = obj.PaneID
			}
		}
	}
	if pane == "" {
		return nil, fmt.Errorf("herdr host: pane split returned no .result.pane.pane_id in %q: check the herdr version", strings.TrimSpace(string(out)))
	}
	hd := &herdrHandle{host: h, pane: pane, files: files, done: make(chan struct{}), watched: make(chan struct{})}

	if _, err := h.herdr("pane", "run", hd.pane, "sh", files.wrapper); err != nil {
		return nil, errors.Join(err, hd.closePane())
	}
	pid, err := h.waitPID(files.pid)
	if err != nil {
		return nil, errors.Join(err, hd.closePane())
	}
	hd.pid = pid

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

func (h *Herdr) waitPID(path string) (int, error) {
	deadline := time.Now().Add(h.startTimeout())
	for {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid, nil
			}
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("herdr host: the wrapper never wrote its pid to %s within %s: check the herdr pane for errors", path, h.startTimeout())
		}
		time.Sleep(h.poll())
	}
}

// wrapperFiles are the files one run uses to talk to mill.
type wrapperFiles struct {
	wrapper, pid, rc, exit string
}

// writeWrapper writes the wrapper script into the log directory. Run as `sh wrapper`, it
// starts itself again as `sh wrapper run` in a new process group (set -m). That inner copy
// writes its pid, runs the command and records its exit code; the outer copy survives a
// kill of the group and writes the final exit code atomically.
func writeWrapper(spec ProcSpec) (wrapperFiles, error) {
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
	files := wrapperFiles{wrapper: f.Name(), pid: base + ".pid", rc: base + ".rc", exit: base + ".exit"}

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
	w, pid, rc, exit := shQuote(files.wrapper), shQuote(files.pid), shQuote(files.rc), shQuote(files.exit)
	script := "#!/bin/sh\n" +
		"# mill herdr wrapper" + strings.ReplaceAll(" "+spec.Name, "\n", " ") + "\n" +
		"if [ \"$1\" = run ]; then\n" +
		"  printf '%s\\n' \"$$\" > " + pid + ".tmp && mv " + pid + ".tmp " + pid + " || exit 1\n" +
		"  { " + cd + strings.Join(cmd, " ") + " < " + shQuote(orNull(spec.StdinPath)) + " 2>> " + shQuote(orNull(spec.StderrPath)) +
		"; printf '%s\\n' \"$?\" > " + rc + ".tmp && mv " + rc + ".tmp " + rc + "; } | tee -a " + shQuote(orNull(spec.StdoutPath)) + "\n" +
		"  exit 0\n" +
		"fi\n" +
		"set -m\n" +
		"sh " + w + " run &\n" +
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
	host  *Herdr
	pane  string
	files wrapperFiles
	pid   int
	done  chan struct{} // closed when Wait has seen the exit
	// closed when the context watcher has returned
	watched chan struct{}

	mu      sync.Mutex
	killed  bool
	killErr error

	closeOnce sync.Once
	closeErr  error
}

func (h *herdrHandle) PID() int { return h.pid }

// closePane closes the pane once; later calls return the first result.
func (h *herdrHandle) closePane() error {
	h.closeOnce.Do(func() {
		_, h.closeErr = h.host.herdr("pane", "close", h.pane)
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

func (h *herdrHandle) waitExit() (int, error) {
	for {
		code, ok, err := h.readExit()
		if err != nil || ok {
			return code, err
		}
		time.Sleep(h.host.poll())
	}
}

func (h *herdrHandle) Wait() (Exit, error) {
	code, err := h.waitExit()
	close(h.done)
	<-h.watched
	if err != nil {
		return Exit{Code: -1}, errors.Join(err, h.closePane())
	}
	// Kill anything the process left running in its group (e.g. backgrounded children).
	killErr := KillGroup(h.pid)
	h.mu.Lock()
	killed := h.killed
	killErr = errors.Join(killErr, h.killErr)
	h.mu.Unlock()
	ex := Exit{Code: code, Signaled: code > 128 || killed}
	return ex, errors.Join(killErr, h.closePane())
}

// Kill stops the process group, waits for the wrapper to record the exit and closes the pane.
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
			return h.closePane()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("herdr host: process group %d killed but %s never appeared: close herdr pane %s by hand", h.pid, h.files.exit, h.pane)
		}
		time.Sleep(h.host.poll())
	}
}
