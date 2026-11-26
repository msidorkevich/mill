package pipeline

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/skiptest"
	"github.com/msidorkevich/mill/internal/store"
)

func skippedTestsEvents(t *testing.T, e *env, id string) []store.Event {
	t.Helper()
	evs, err := e.p.Store.TaskEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Event
	for _, ev := range evs {
		if ev.Kind == SkippedTestsEvent || ev.Kind == OldSkippedTestsEvent {
			out = append(out, ev)
		}
	}
	return out
}

func TestSkippedTestsAddedByTaskAreRecordedOnVerification(t *testing.T) {
	t.Parallel()
	src := "package x\n\nimport \"testing\"\n\nfunc TestHello(t *testing.T) {\n\tt.Skip(\"later\")\n}\n"
	e := setup(t, `grep -q hello hello.txt`, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n", "x/hello_test.go": src})},
		runner.RoleVerifier:    {verdict("pass")},
	}, nil)
	e.task(t, "hello")
	out := e.run(t, "hello")
	if out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	evs := skippedTestsEvents(t, e, "hello")
	if len(evs) != 1 || evs[0].Kind != "skipped-tests" {
		t.Fatalf("skipped-tests events = %+v, want one of kind skipped-tests", evs)
	}
	var data struct {
		Head  string          `json:"head"`
		Skips []skiptest.Skip `json:"skips"`
	}
	if err := json.Unmarshal(evs[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	want := skiptest.Skip{File: "x/hello_test.go", Line: 6, Reason: "later"}
	if len(data.Skips) != 1 || data.Skips[0] != want || data.Head != out.HeadSHA {
		t.Fatalf("event data = %s", evs[0].Data)
	}
}

func TestSkippedTestsNotRecordedWithoutAddedSkips(t *testing.T) {
	t.Parallel()
	src := "package x\n\nimport \"testing\"\n\nfunc TestHello(t *testing.T) {\n}\n"
	e := setup(t, `grep -q hello hello.txt`, runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n", "x/hello_test.go": src})},
		runner.RoleVerifier:    {verdict("pass")},
	}, nil)
	e.task(t, "hello")
	if out := e.run(t, "hello"); out.Status != store.TaskVerified {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	if evs := skippedTestsEvents(t, e, "hello"); len(evs) != 0 {
		t.Fatalf("skipped-tests events = %+v, want none", evs)
	}
}

func TestSkippedTestsClearedWhenLaterAttemptVerifiesWithoutSkips(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{SkippedTestsEvent, OldSkippedTestsEvent} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			src := "package x\n\nimport \"testing\"\n\nfunc TestHello(t *testing.T) {\n}\n"
			e := setup(t, `grep -q hello hello.txt`, runner.Script{
				runner.RoleImplementer: {impl(map[string]string{"hello.txt": "hello\n", "x/hello_test.go": src})},
				runner.RoleVerifier:    {verdict("pass")},
			}, nil)
			e.task(t, "hello")
			// An earlier attempt was verified with a skip that the task no longer adds.
			old := map[string]any{"attempt": 1, "head": "deadbeef", "skips": []skiptest.Skip{{File: "x/hello_test.go", Line: 6}}}
			if _, err := e.p.Store.AppendEvent(context.Background(), kind, "", "hello", old); err != nil {
				t.Fatal(err)
			}
			out := e.run(t, "hello")
			if out.Status != store.TaskVerified {
				t.Fatalf("outcome = %+v\n%s", out, e.out)
			}
			evs := skippedTestsEvents(t, e, "hello")
			if len(evs) != 2 || evs[1].Kind != SkippedTestsEvent {
				t.Fatalf("skipped-tests events = %+v, want the old one and an empty one", evs)
			}
			var data struct {
				Head  string          `json:"head"`
				Skips []skiptest.Skip `json:"skips"`
			}
			if err := json.Unmarshal(evs[1].Data, &data); err != nil {
				t.Fatal(err)
			}
			if len(data.Skips) != 0 || data.Head != out.HeadSHA {
				t.Fatalf("clearing event data = %s", evs[1].Data)
			}
		})
	}
}
