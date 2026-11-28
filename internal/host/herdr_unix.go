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
	// KeepFailed is how long the tab of a failed worker (non-zero exit or lost attempt)
	// stays open after Wait; 0 = close at once. Successful and killed workers always close
	// at once. The delayed close needs this process to still be running.
	KeepFailed time.Duration
	// AfterFunc schedules the delayed close; nil = time.AfterFunc.
	AfterFunc func(time.Duration, func())
	// Errors receives errors of delayed closes; nil = os.Stderr. It must be safe for
	// concurrent use.
	Errors io.Writer
}

// NewHerdr returns the herdr host using the herdr binary on PATH and the workspace in
// HERDR_WORKSPACE_ID.
func NewHerdr() *Herdr {
	return &Herdr{Bin: "herdr", Workspace: os.Getenv("HERDR_WORKSPACE_ID"), Poll: 50 * time.Millisecond, StartTimeout: 30 * time.Second, Grace: 2 * time.Second}
}

// Name implements Host.
func (*Herdr) Name() string { return "herdr" }

func (h *Herdr) grace() time.Duration { return orDefault(h.Grace, defaultGrace) }

func (h *Herdr) poll() time.Duration { return orDefault(h.Poll, defaultPoll) }

func (h *Herdr) startTimeout() time.Duration { return orDefault(h.StartTimeout, defaultStartTimeout) }

// pgrun returns the argv prefix that starts the worker in a new process group.
func (h *Herdr) pgrun() ([]string, error) { return pgrunPrefix("herdr", h.Pgrun) }

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

// herdrHandle is a worker running in a herdr tab.
type herdrHandle struct{ *wrapperHandle }

// Start implements Host.
func (h *Herdr) Start(ctx context.Context, spec ProcSpec) (Handle, error) {
	cfg := wrapperConfig{poll: h.poll(), startTimeout: h.startTimeout(), grace: h.grace(), pgrun: h.Pgrun,
		keepFailed: h.KeepFailed, afterFunc: h.AfterFunc, errors: h.Errors}
	hd, err := startWrapped(ctx, herdrTerminal{h}, cfg, spec)
	if err != nil {
		return nil, err
	}
	return &herdrHandle{hd}, nil
}

// herdrTerminal opens worker windows as herdr tabs.
type herdrTerminal struct{ h *Herdr }

func (herdrTerminal) kind() string { return "herdr" }

// open creates a tab, records it in the tab file and runs argv in the tab's root pane.
func (t herdrTerminal) open(label, dir string, argv []string, record string) (window, error) {
	// Each worker gets its own tab, never --focus: the operator's focus stays where it is.
	args := []string{"tab", "create"}
	if t.h.Workspace != "" {
		args = append(args, "--workspace", t.h.Workspace)
	}
	args = append(args, "--label", label, "--cwd", dir)
	out, err := t.h.herdr(args...)
	if err != nil {
		return nil, err
	}
	pane, tab, err := parseTabCreate(out)
	if err != nil {
		return nil, err
	}
	w := &herdrWindow{host: t.h, tab: tab, file: record}
	// The tab file lets crash recovery close a tab a killed mill left open; the mill pid
	// tells recovery whether the mill that started the worker is still running.
	if err := writeTabFile(record, HerdrTab{Tab: tab, Workspace: t.h.Workspace, MillPID: os.Getpid()}); err != nil {
		return nil, errors.Join(err, w.close())
	}
	if _, err := t.h.herdr(append([]string{"pane", "run", pane}, argv...)...); err != nil {
		return nil, errors.Join(err, w.close())
	}
	return w, nil
}

// herdrWindow is a worker's herdr tab and the tab file recording it.
type herdrWindow struct {
	host *Herdr
	tab  string
	file string
}

// close closes the tab. A closed tab's tab file goes with it, so recovery never closes a
// later tab that reuses its id.
func (w *herdrWindow) close() error {
	if _, err := w.host.herdr("tab", "close", w.tab); err != nil {
		return err
	}
	if err := os.Remove(w.file); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("herdr host: remove tab file of closed tab %s: %w; delete it by hand", w.tab, err)
	}
	return nil
}

// exists reports true: herdr is not asked, a closed tab kills the wrapper running in it.
func (*herdrWindow) exists() (bool, error) { return true, nil }

func (w *herdrWindow) String() string { return "herdr tab " + w.tab }

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
