//go:build unix

package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Tmux runs each worker in its own new detached tmux window. Like the herdr host, the window
// only shows the worker: stdout, stderr, the pid and the exit code all go through files
// written by the shared wrapper script, so results never depend on the window.
type Tmux struct {
	Bin string // tmux binary; "tmux" resolves on PATH
	// Socket is the tmux server socket name, passed as `-L <socket>` on every call; "" = the
	// default server.
	Socket string
	// Session is the tmux session worker windows open in; "" = decided at Start: inside tmux
	// the session of $TMUX_PANE, outside it TmuxSessionName(RepoRoot), created when missing.
	Session string
	// RepoRoot names and roots the detached session used outside tmux; "" = the working
	// directory.
	RepoRoot     string
	Poll         time.Duration // how often pid and exit files are polled
	StartTimeout time.Duration // how long Start waits for the pid file
	Grace        time.Duration // how long Wait waits for the exit file once the wrapper is gone
	// Pgrun is the argv prefix that runs a command as the leader of a new process group, in
	// place; empty = the running mill binary with `__pgrun --`.
	Pgrun []string
	// KeepFailed is how long the window of a failed worker (non-zero exit or lost attempt)
	// stays open after Wait; 0 = close at once. Successful and killed workers always close
	// at once. The delayed close needs this process to still be running; tmux itself closes
	// a window whose command ended unless its remain-on-exit option is on.
	KeepFailed time.Duration
	// AfterFunc schedules the delayed close; nil = time.AfterFunc.
	AfterFunc func(time.Duration, func())
	// Errors receives errors of delayed closes; nil = os.Stderr. It must be safe for
	// concurrent use.
	Errors io.Writer
	// Out receives the hint to attach to a detached session, once per Tmux; nil = os.Stderr.
	Out io.Writer
	// Getenv reads TMUX and TMUX_PANE; nil = os.Getenv.
	Getenv func(string) string

	mu   sync.Mutex // serialises session lookup and creation
	hint sync.Once  // prints the attach hint once
}

// NewTmux returns the tmux host using the tmux binary on PATH and its default server.
func NewTmux() *Tmux {
	return &Tmux{Bin: "tmux", Poll: defaultPoll, StartTimeout: defaultStartTimeout, Grace: defaultGrace, Out: os.Stderr, Getenv: os.Getenv}
}

// Name implements Host.
func (*Tmux) Name() string { return "tmux" }

// TmuxSessionName is the detached session mill uses outside tmux for the repository at
// repoRoot: mill-<base name>, with '.' and ':' (which tmux rejects) replaced by '-'.
func TmuxSessionName(repoRoot string) string {
	return "mill-" + strings.NewReplacer(".", "-", ":", "-").Replace(filepath.Base(repoRoot))
}

// tmuxArgs prefixes args with the socket selection.
func tmuxArgs(socket string, args ...string) []string {
	if socket == "" {
		return args
	}
	return append([]string{"-L", socket}, args...)
}

// tmux runs the tmux binary and returns its stdout. A failure includes tmux's stderr.
func (t *Tmux) tmux(args ...string) ([]byte, error) {
	bin := t.Bin
	if bin == "" {
		bin = "tmux"
	}
	args = tmuxArgs(t.Socket, args...)
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, &tmuxError{call: bin + " " + strings.Join(args, " "), err: err, stderr: strings.TrimSpace(stderr.String())}
	}
	return stdout.Bytes(), nil
}

// tmuxError is a failed tmux call.
type tmuxError struct {
	call   string
	err    error
	stderr string
}

func (e *tmuxError) Error() string {
	return fmt.Sprintf("tmux host: %s: %v: %s", e.call, e.err, e.stderr)
}

func (e *tmuxError) Unwrap() error { return e.err }

// missing reports whether tmux said the target or its server does not exist.
func (e *tmuxError) missing() bool { return tmuxMissing(e.stderr) }

// tmuxMissing reports whether tmux's output says the target, or the whole server, is gone.
func tmuxMissing(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range []string{"can't find", "no server running", "no such", "not found", "error connecting to"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// session returns the session worker windows open in, creating the detached session
// outside tmux when it does not exist yet. The name is prefixed with '=' so tmux matches it
// exactly, never as a prefix of another session's name.
func (t *Tmux) session() (string, error) {
	if t.Session != "" {
		return "=" + t.Session, nil
	}
	getenv := t.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if getenv("TMUX") != "" {
		args := []string{"display-message", "-p"}
		if pane := getenv("TMUX_PANE"); pane != "" {
			args = append(args, "-t", pane)
		}
		out, err := t.tmux(append(args, "#{session_name}")...)
		if err != nil {
			return "", fmt.Errorf("%w; mill runs inside tmux ($TMUX is set) but could not find its session: unset TMUX or set host.kind = \"exec\"", err)
		}
		name := strings.TrimSpace(string(out))
		if name == "" {
			return "", errors.New(`tmux host: display-message printed no session name for $TMUX_PANE: check the tmux version or set host.kind = "exec"`)
		}
		return "=" + name, nil
	}

	root := t.RepoRoot
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("tmux host: working directory to name the session: %w", err)
		}
		root = wd
	}
	name := TmuxSessionName(root)
	t.mu.Lock()
	defer t.mu.Unlock()
	// has-session fails when the session (or the whole server) does not exist yet; any other
	// problem makes new-session fail too, with tmux's own message.
	if _, err := t.tmux("has-session", "-t", "="+name); err != nil {
		if _, err := t.tmux("new-session", "-d", "-s", name, "-c", root); err != nil {
			return "", fmt.Errorf("%w; create the session by hand with `tmux new-session -d -s %s` or set host.kind = \"exec\"", err, name)
		}
	}
	t.hint.Do(func() {
		out := t.Out
		if out == nil {
			out = os.Stderr
		}
		attach := "tmux"
		if t.Socket != "" {
			attach += " -L " + shQuote(t.Socket)
		}
		fmt.Fprintf(out, "mill: workers run in tmux session %s; watch them with: %s attach -t %s\n", name, attach, name)
	})
	return "=" + name, nil
}

// tmuxHandle is a worker running in a tmux window.
type tmuxHandle struct{ *wrapperHandle }

// Start implements Host.
func (t *Tmux) Start(ctx context.Context, spec ProcSpec) (Handle, error) {
	cfg := wrapperConfig{poll: orDefault(t.Poll, defaultPoll), startTimeout: orDefault(t.StartTimeout, defaultStartTimeout),
		grace: orDefault(t.Grace, defaultGrace), pgrun: t.Pgrun, keepFailed: t.KeepFailed, afterFunc: t.AfterFunc, errors: t.Errors}
	hd, err := startWrapped(ctx, tmuxTerminal{t}, cfg, spec)
	if err != nil {
		return nil, err
	}
	return &tmuxHandle{hd}, nil
}

// tmuxTerminal opens worker windows as detached tmux windows.
type tmuxTerminal struct{ t *Tmux }

func (tmuxTerminal) kind() string { return "tmux" }

// tmuxRecordPath returns the tmux-*.window file next to the wrapper's files for the record
// path the wrapper protocol names.
func tmuxRecordPath(record string) string {
	dir, name := filepath.Split(record)
	name = strings.TrimSuffix(strings.TrimPrefix(name, "herdr-"), filepath.Ext(name))
	return filepath.Join(dir, "tmux-"+name+".window")
}

// open creates a detached window labelled label running argv and records it in a window
// file. The window is never selected: the operator's focus stays where it is.
func (tt tmuxTerminal) open(label, dir string, argv []string, record string) (window, error) {
	session, err := tt.t.session()
	if err != nil {
		return nil, err
	}
	// One shell-command argument: every tmux version runs it through the shell, so the
	// quoting holds for paths with spaces.
	words := make([]string, len(argv))
	for i, a := range argv {
		words[i] = shQuote(a)
	}
	out, err := tt.t.tmux("new-window", "-d", "-P", "-F", "#{window_id}", "-n", label, "-c", dir, "-t", session+":", strings.Join(words, " "))
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(string(out))
	if !strings.HasPrefix(id, "@") || strings.ContainsAny(id, " \n") {
		return nil, fmt.Errorf("tmux host: new-window printed %q, not a window id like @3: check the tmux version; close the worker's window %q by hand", id, label)
	}
	w := &tmuxWindow{host: tt.t, id: id, file: tmuxRecordPath(record)}
	// The window file lets crash recovery close a window a killed mill left open; the mill
	// pid tells recovery whether the mill that started the worker is still running.
	rec := TmuxWindow{Window: id, Session: strings.TrimPrefix(session, "="), Socket: tt.t.Socket, MillPID: os.Getpid()}
	if err := writeWindowFile(w.file, rec); err != nil {
		return nil, errors.Join(err, w.close())
	}
	return w, nil
}

// tmuxWindow is a worker's tmux window and the window file recording it.
type tmuxWindow struct {
	host *Tmux
	id   string
	file string
}

// close kills the window; a window that no longer exists is already closed. A closed
// window's file goes with it, so recovery never closes a later window that reuses its id.
func (w *tmuxWindow) close() error {
	if _, err := w.host.tmux("kill-window", "-t", w.id); err != nil {
		var te *tmuxError
		if !errors.As(err, &te) || !te.missing() {
			return fmt.Errorf("%w; close tmux window %s by hand", err, w.id)
		}
	}
	if err := os.Remove(w.file); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("tmux host: remove window file of closed window %s: %w; delete it by hand", w.id, err)
	}
	return nil
}

// exists asks tmux whether the window is still open.
func (w *tmuxWindow) exists() (bool, error) {
	out, err := w.host.tmux("list-windows", "-a", "-F", "#{window_id}")
	if err != nil {
		var te *tmuxError
		if errors.As(err, &te) && te.missing() {
			return false, nil
		}
		return false, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == w.id {
			return true, nil
		}
	}
	return false, nil
}

func (w *tmuxWindow) String() string { return "tmux window " + w.id }

// TmuxWindow is a worker window recorded in a tmux-*.window file next to the wrapper's files.
type TmuxWindow struct {
	Window  string `json:"window"`  // window id, e.g. "@3"
	Session string `json:"session"` // session the window opened in
	Socket  string `json:"socket"`  // tmux socket name (-L); "" = the default server
	// MillPID is the pid of the mill process that started the worker.
	MillPID int `json:"mill_pid,omitempty"`
	// Bin is the tmux binary CloseTmuxWindow runs; "" resolves "tmux" on PATH. It is not
	// recorded: the caller sets it the way it selects the tmux host.
	Bin string `json:"-"`
	// File is the tmux-*.window file the window was read from.
	File string `json:"-"`
}

// writeWindowFile records w in path as JSON.
func writeWindowFile(path string, w TmuxWindow) error {
	b, err := json.Marshal(w)
	if err != nil {
		return fmt.Errorf("tmux host: window file %s: %w", path, err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("tmux host: write window file %s: %w; the worker's window was closed", path, err)
	}
	return nil
}

// RecordedTmuxWindows returns the windows recorded in every tmux-*.window file in dir, in
// file name order. A missing dir holds none.
func RecordedTmuxWindows(dir string) ([]TmuxWindow, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("tmux host: read recorded windows in %s: %w", dir, err)
	}
	var windows []TmuxWindow
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "tmux-") || !strings.HasSuffix(name, ".window") {
			continue
		}
		path := filepath.Join(dir, name)
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("tmux host: read window file %s: %w", path, err)
		}
		var w TmuxWindow
		if err := json.Unmarshal(b, &w); err != nil || w.Window == "" {
			return nil, fmt.Errorf("tmux host: window file %s holds %q, not {\"window\": ..., \"session\": ..., \"socket\": ...}: close the window by hand and delete the file", path, strings.TrimSpace(string(b)))
		}
		w.File = path
		windows = append(windows, w)
	}
	return windows, nil
}

// CloseTmuxWindow runs `tmux [-L <socket>] kill-window -t <window>`. It returns closed=false
// and no error when tmux is not installed or reports that the window, or its server, no
// longer exists.
func CloseTmuxWindow(w TmuxWindow) (closed bool, err error) {
	bin := w.Bin
	if bin == "" {
		if bin, err = exec.LookPath("tmux"); err != nil {
			return false, nil
		}
	}
	args := tmuxArgs(w.Socket, "kill-window", "-t", w.Window)
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		msg := strings.TrimSpace(stderr.String() + " " + stdout.String())
		if tmuxMissing(msg) {
			return false, nil
		}
		return false, fmt.Errorf("tmux host: %s %s: %w: %s; close tmux window %s by hand", bin, strings.Join(args, " "), err, msg, w.Window)
	}
	return true, nil
}
