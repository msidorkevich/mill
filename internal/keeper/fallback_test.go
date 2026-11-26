package keeper

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/pipeline"
)

func TestFallbackCombinedCheckRunsOnExecWithNote(t *testing.T) {
	t.Parallel()
	marker := filepath.Join(t.TempDir(), "ran")
	e := combinedSetup(t, "echo ran > '"+marker+"'", []string{"a", "b"}, nil)
	e.o.Host = failingHost{}
	e.o.Pipeline = &pipeline.Pipeline{Store: e.o.Store, Cfg: e.o.Cfg, Paths: e.o.Paths, Home: t.TempDir(),
		GateSlotPoll: time.Millisecond}

	res, err := Combined(context.Background(), e.o)
	if err != nil || !res.Ran || !res.Passed {
		t.Fatalf("Combined = %+v, %v; want a pass on exec\n%s", res, err, e.out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("check_cmd did not run: %v", err)
	}
	out := e.out.String()
	if !strings.Contains(out, "failing could not start it") || !strings.Contains(out, "ran on exec") {
		t.Fatalf("output lacks the fallback note:\n%s", out)
	}
}
