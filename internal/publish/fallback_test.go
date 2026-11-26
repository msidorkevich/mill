package publish

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/gate"
	"github.com/msidorkevich/mill/internal/host"
)

// fallbackHost cannot start any process.
type fallbackHost struct{}

func (fallbackHost) Name() string { return "broken" }
func (fallbackHost) Start(context.Context, host.ProcSpec) (host.Handle, error) {
	return nil, errors.New("no such host")
}

func TestFallbackRestackGateRunsOnExecWithNote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o, _, br := fixture(t)
	marker := filepath.Join(t.TempDir(), "ran")
	o.Cfg.CheckCmd = "echo ran > '" + marker + "'"
	o.Host = fallbackHost{}
	o.GateSlotPoll = time.Millisecond
	tk, err := o.Store.GetTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}

	g, err := GateAtHead(ctx, o, tk, br, "sync")
	if err != nil || !g.Passed {
		t.Fatalf("gate = %+v, %v; want a pass on exec", g, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("check_cmd did not run: %v", err)
	}

	raw, err := json.Marshal(gateReuseLastGateEvent(t, o.Store, "t1")["result"])
	if err != nil {
		t.Fatal(err)
	}
	var recorded gate.Result
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatal(err)
	}
	if len(recorded.Commands) == 0 {
		t.Fatalf("recorded gate = %+v, want its commands", recorded)
	}
	note := recorded.Commands[0].Note
	if !strings.Contains(note, "broken could not start it") || !strings.HasSuffix(note, "ran on exec") {
		t.Fatalf("recorded note = %q", note)
	}
}

func TestFallbackPublishGateRunsOnExec(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o, _, br := fixture(t)
	marker := filepath.Join(t.TempDir(), "ran")
	o.Cfg.CheckCmd = "echo ran > '" + marker + "'"
	o.Host = fallbackHost{}
	o.GateSlotPoll = time.Millisecond
	br.GateSHA = br.BaseSHA // the gate did not run at the head, so publishing re-runs it
	if err := o.Store.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}

	res, err := Publish(ctx, o)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.GateRerun == nil || !res.GateRerun.Passed || !strings.HasSuffix(res.GateRerun.Commands[0].Note, "ran on exec") {
		t.Fatalf("GateRerun = %+v, want a pass on exec with a note", res.GateRerun)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("check_cmd did not run: %v", err)
	}
}
