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

// herdrTabs is what recovery needs to close the herdr tabs killed workers left open.
type herdrTabs struct {
	runs     string                       // the project's runs directory; "" = no tabs to close
	lookPath func(string) (string, error) // finds herdr; nil = exec.LookPath
}

// attemptRunDir is the attempt's run directory, derived like the pipeline's attempt LogDir.
func attemptRunDir(runs string, a store.Attempt) string {
	return filepath.Join(runs, a.TaskID, fmt.Sprintf("a%d", a.N))
}

// closeRecordedTabs closes the herdr tabs recorded in the attempt's run directory and
// prints a line for each tab it closed. A tab whose recorded mill process is still alive
// belongs to a running mill and stays open. Tabs already gone, and herdr not being
// installed, are not errors and print nothing.
func closeRecordedTabs(out io.Writer, tabs herdrTabs, a store.Attempt) error {
	if tabs.runs == "" {
		return nil
	}
	what := fmt.Sprintf("%s attempt %d %s", a.TaskID, a.N, a.Role)
	recorded, err := host.RecordedHerdrTabs(attemptRunDir(tabs.runs, a))
	if err != nil {
		return fmt.Errorf("recover herdr tabs of %s: %w", what, err)
	}
	var orphaned []host.HerdrTab
	for _, t := range recorded {
		alive, err := millAlive(t)
		if err != nil {
			return fmt.Errorf("recover herdr tabs of %s: %w", what, err)
		}
		if !alive {
			orphaned = append(orphaned, t)
		}
	}
	return closeTabs(out, tabs, orphaned, func(host.HerdrTab) string { return what })
}

// closeOrphanedTabs closes every herdr tab recorded anywhere under the runs directory
// (pipeline attempts, keeper, sync and combined-check dirs) whose recorded mill process is
// gone, and prints a line for each tab it closed. Tab files without a mill pid are left to
// closeRecordedTabs, which closes them only for a running attempt.
func closeOrphanedTabs(out io.Writer, tabs herdrTabs) error {
	if tabs.runs == "" {
		return nil
	}
	var orphaned []host.HerdrTab
	err := filepath.WalkDir(tabs.runs, func(path string, d fs.DirEntry, err error) error {
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
		recorded, err := host.RecordedHerdrTabs(path)
		if err != nil {
			return err
		}
		for _, t := range recorded {
			if t.MillPID <= 0 {
				continue
			}
			alive, err := millAlive(t)
			if err != nil {
				return err
			}
			if !alive {
				orphaned = append(orphaned, t)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("recover orphaned herdr tabs under %s: %w", tabs.runs, err)
	}
	return closeTabs(out, tabs, orphaned, func(t host.HerdrTab) string {
		rel, err := filepath.Rel(tabs.runs, filepath.Dir(t.File))
		if err != nil {
			rel = filepath.Dir(t.File)
		}
		return fmt.Sprintf("%s (mill %d is gone)", rel, t.MillPID)
	})
}

// millAlive reports whether the mill process that recorded t is still running. A tab file
// without a mill pid has no live owner.
func millAlive(t host.HerdrTab) (bool, error) {
	if t.MillPID <= 0 {
		return false, nil
	}
	_, _, alive, err := host.ProcessInfo(t.MillPID)
	if err != nil {
		return false, fmt.Errorf("check mill process %d of herdr tab %s (%s): %w; close the tab by hand if its mill is gone", t.MillPID, t.Tab, t.File, err)
	}
	return alive, nil
}

// closeTabs closes each tab, removes its tab file and prints a line naming it with what(t)
// for each tab it closed. herdr not being installed closes nothing and keeps the files.
func closeTabs(out io.Writer, tabs herdrTabs, recorded []host.HerdrTab, what func(host.HerdrTab) string) error {
	if len(recorded) == 0 {
		return nil
	}
	lookPath := tabs.lookPath
	if lookPath == nil {
		lookPath = Env{}.lookPath
	}
	bin, err := lookPath("herdr")
	if err != nil {
		// herdr is not installed: there is no tab to close, and the files stay for a later
		// recovery that has herdr.
		return nil
	}
	for _, t := range recorded {
		t.Bin = bin
		closed, err := host.CloseHerdrTab(t)
		if err != nil {
			return fmt.Errorf("recover herdr tabs of %s: %w", what(t), err)
		}
		// The tab is gone either way: drop its file so a later tab reusing the id is safe.
		if err := os.Remove(t.File); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove tab file %s of %s: %w; delete it by hand", t.File, what(t), err)
		}
		if closed {
			fmt.Fprintf(out, "closed herdr tab %s of %s\n", t.Tab, what(t))
		}
	}
	return nil
}
