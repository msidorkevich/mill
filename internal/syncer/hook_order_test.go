package syncer

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// hookOrderRuns counts the hook-run events of task id.
func hookOrderRuns(t *testing.T, o Options, id string) int {
	t.Helper()
	evs, err := o.Store.TaskEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range evs {
		if ev.Kind == "hook-run" {
			n++
		}
	}
	return n
}

func TestHookLineAfterPublishedSyncRepublish(t *testing.T) {
	t.Parallel()
	o, _ := publishedStack(t)
	o.Cfg.OnUpdate = "echo https://example.test/pr/1"
	var out bytes.Buffer
	o.Out = &out
	res, err := Sync(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Republished) != 1 {
		t.Fatalf("result = %+v", res)
	}
	s := out.String()
	progress := strings.Index(s, "g1/b: republished ")
	hookLine := strings.Index(s, "hook on_update for g1/b: https://example.test/pr/1\n")
	if progress < 0 || hookLine < 0 {
		t.Fatalf("output lacks the republished line or the hook line:\n%s", s)
	}
	if hookLine < progress {
		t.Errorf("the hook line came before the republished line:\n%s", s)
	}
	if n := hookOrderRuns(t, o, "g1/b"); n != 1 {
		t.Errorf("%d hook-run events, want 1", n)
	}
}
