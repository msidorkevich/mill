package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/store"
)

// hostWindows is what recovery needs to close the herdr tabs and tmux windows killed
// workers left open.
type hostWindows struct {
	runs     string                       // the project's runs directory; "" = no windows to close
	lookPath func(string) (string, error) // finds herdr and tmux; nil = exec.LookPath
	tmuxBin  string                       // tmux executable (host.tmux_bin), looked up on PATH; "" = "tmux"
}

// recordedWindow is a worker window recorded in a run directory by either host: a herdr tab
// or a tmux window.
type recordedWindow struct {
	file    string // the record's file
	id      string // the tab or window id
	millPID int    // the mill process that started the worker; 0 = not recorded
	kind    string // "herdr tab" or "tmux window", for messages
	bin     string // the executable that closes it, looked up through lookPath
	// close closes the window with the resolved executable; closed=false and no error
	// when the window no longer exists.
	close func(bin string) (closed bool, err error)
}

func (w recordedWindow) describe() string { return w.kind + " " + w.id }

// recordedWindows returns the herdr tabs and tmux windows recorded in dir.
func recordedWindows(dir string, windows hostWindows) ([]recordedWindow, error) {
	tabs, err := host.RecordedHerdrTabs(dir)
	if err != nil {
		return nil, err
	}
	tmuxWindows, err := host.RecordedTmuxWindows(dir)
	if err != nil {
		return nil, err
	}
	var recorded []recordedWindow
	for _, t := range tabs {
		recorded = append(recorded, recordedWindow{
			file: t.File, id: t.Tab, millPID: t.MillPID, kind: "herdr tab", bin: "herdr",
			close: func(bin string) (bool, error) { t.Bin = bin; return host.CloseHerdrTab(t) },
		})
	}
	tmuxBin := windows.tmuxBin
	if tmuxBin == "" {
		tmuxBin = "tmux"
	}
	for _, w := range tmuxWindows {
		recorded = append(recorded, recordedWindow{
			file: w.File, id: w.Window, millPID: w.MillPID, kind: "tmux window", bin: tmuxBin,
			close: func(bin string) (bool, error) { w.Bin = bin; return host.CloseTmuxWindow(w) },
		})
	}
	return recorded, nil
}

// attemptRunDir is the attempt's run directory, derived like the pipeline's attempt LogDir.
func attemptRunDir(runs string, a store.Attempt) string {
	return filepath.Join(runs, a.TaskID, fmt.Sprintf("a%d", a.N))
}

// closeRecordedTabs closes the herdr tabs and tmux windows recorded in the attempt's run
// directory and prints a line for each one it closed. A window whose recorded mill process
// is still alive belongs to a running mill and stays open. Windows already gone, and the
// host's binary not being installed, are not errors and print nothing.
func closeRecordedTabs(out io.Writer, windows hostWindows, a store.Attempt) error {
	if windows.runs == "" {
		return nil
	}
	what := fmt.Sprintf("%s attempt %d %s", a.TaskID, a.N, a.Role)
	recorded, err := recordedWindows(attemptRunDir(windows.runs, a), windows)
	if err != nil {
		return fmt.Errorf("recover worker windows of %s: %w", what, err)
	}
	var orphaned []recordedWindow
	for _, w := range recorded {
		alive, err := millAlive(w)
		if err != nil {
			return fmt.Errorf("recover worker windows of %s: %w", what, err)
		}
		if !alive {
			orphaned = append(orphaned, w)
		}
	}
	return closeWindows(out, windows, orphaned, func(recordedWindow) string { return what })
}

// closeOrphanedTabs closes every herdr tab and tmux window recorded anywhere under the runs
// directory (pipeline attempts, keeper, sync and combined-check dirs) whose recorded mill
// process is gone, and prints a line for each one it closed. Records without a mill pid are
// left to closeRecordedTabs, which closes them only for a running attempt.
func closeOrphanedTabs(out io.Writer, windows hostWindows) error {
	if windows.runs == "" {
		return nil
	}
	var orphaned []recordedWindow
	err := filepath.WalkDir(windows.runs, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			// No runs yet, or a run dir removed while recovery walks: nothing to close there.
			return nil
		}
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		recorded, err := recordedWindows(path, windows)
		if err != nil {
			return err
		}
		for _, w := range recorded {
			if w.millPID <= 0 {
				continue
			}
			alive, err := millAlive(w)
			if err != nil {
				return err
			}
			if !alive {
				orphaned = append(orphaned, w)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("recover orphaned worker windows under %s: %w", windows.runs, err)
	}
	return closeWindows(out, windows, orphaned, func(w recordedWindow) string {
		rel, err := filepath.Rel(windows.runs, filepath.Dir(w.file))
		if err != nil {
			rel = filepath.Dir(w.file)
		}
		return fmt.Sprintf("%s (mill %d is gone)", rel, w.millPID)
	})
}

// millAlive reports whether the mill process that recorded w is still running. A record
// without a mill pid has no live owner.
func millAlive(w recordedWindow) (bool, error) {
	if w.millPID <= 0 {
		return false, nil
	}
	_, _, alive, err := host.ProcessInfo(w.millPID)
	if err != nil {
		return false, fmt.Errorf("check mill process %d of %s (%s): %w; close the %s by hand if its mill is gone", w.millPID, w.describe(), w.file, err, w.kind)
	}
	return alive, nil
}

// closeWindows closes each window, removes its record and prints a line naming it with
// what(w) for each window it closed. A host binary that is not installed closes nothing of
// that host and keeps its files.
func closeWindows(out io.Writer, windows hostWindows, recorded []recordedWindow, what func(recordedWindow) string) error {
	if len(recorded) == 0 {
		return nil
	}
	lookPath := windows.lookPath
	if lookPath == nil {
		lookPath = Env{}.lookPath
	}
	bins := map[string]string{} // executable name -> path; "" = not installed
	for _, w := range recorded {
		bin, looked := bins[w.bin]
		if !looked {
			// A lookup failure means the binary is not installed: there is no window to
			// close, and the files stay for a later recovery that has it.
			path, err := lookPath(w.bin)
			if err != nil {
				path = ""
			}
			bin, bins[w.bin] = path, path
		}
		if bin == "" {
			continue
		}
		closed, err := w.close(bin)
		if err != nil {
			return fmt.Errorf("recover %ss of %s: %w", w.kind, what(w), err)
		}
		// The window is gone either way: drop its record so a later window reusing the id
		// is safe.
		if err := os.Remove(w.file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s file %s of %s: %w; delete it by hand", w.kind, w.file, what(w), err)
		}
		if closed {
			fmt.Fprintf(out, "closed %s of %s\n", w.describe(), what(w))
		}
	}
	return nil
}
