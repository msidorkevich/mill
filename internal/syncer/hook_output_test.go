package syncer

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestSyncRepublishPrintsHookSuccessLine(t *testing.T) {
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
	want := "hook on_update for g1/b: https://example.test/pr/1\n"
	if n := strings.Count(out.String(), want); n != 1 {
		t.Errorf("output has %d lines %q, want 1:\n%s", n, want, out.String())
	}
}
