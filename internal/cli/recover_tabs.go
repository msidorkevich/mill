package cli

import (
	"errors"
	"fmt"
	"io"
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

// closeRecordedTabs closes every herdr tab recorded in the attempt's run directory and
// prints a line for each tab it closed. Tabs already gone, and herdr not being installed,
// are not errors and print nothing.
func closeRecordedTabs(out io.Writer, tabs herdrTabs, a store.Attempt) error {
	if tabs.runs == "" {
		return nil
	}
	recorded, err := host.RecordedHerdrTabs(attemptRunDir(tabs.runs, a))
	if err != nil {
		return fmt.Errorf("recover herdr tabs of %s attempt %d %s: %w", a.TaskID, a.N, a.Role, err)
	}
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
			return fmt.Errorf("recover herdr tabs of %s attempt %d %s: %w", a.TaskID, a.N, a.Role, err)
		}
		// The tab is gone either way: drop its file so a later tab reusing the id is safe.
		if err := os.Remove(t.File); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove tab file %s of %s attempt %d %s: %w; delete it by hand", t.File, a.TaskID, a.N, a.Role, err)
		}
		if closed {
			fmt.Fprintf(out, "closed herdr tab %s of %s attempt %d %s\n", t.Tab, a.TaskID, a.N, a.Role)
		}
	}
	return nil
}
