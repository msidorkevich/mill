package hook

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

const validStatus = `{"schema":"mill/status@1","state":"failure","url":"https://example.test/pr/1","checks":[{"name":"ci","state":"failure","url":"https://example.test/ci","summary":"tests failed"},{"name":"lint","state":"success","summary":""}]}`

func TestStatusRunParsesStdoutOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := hookConfig()
	cfg.Status = "echo \"$MILL_HOOK_EVENT\" > '" + f.path("event") + "'; echo '{\"noise\":1}' >&2; echo 'progress...'; echo '" + validStatus + "'; echo 'stderr noise' >&2"

	res, err := f.runner(cfg, 5*time.Millisecond).Run(context.Background(), EventStatus, "g1", "g1/a", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed() || res.Status == nil {
		t.Fatalf("result = %+v, want a parsed status", res)
	}
	want := Status{Schema: "mill/status@1", State: "failure", URL: "https://example.test/pr/1", Checks: []StatusCheck{
		{Name: "ci", State: "failure", URL: "https://example.test/ci", Summary: "tests failed"},
		{Name: "lint", State: "success"},
	}}
	if !reflect.DeepEqual(*res.Status, want) {
		t.Errorf("status = %+v\nwant     %+v", *res.Status, want)
	}
	if strings.Contains(res.Stdout, "noise") || !strings.Contains(res.Stdout, "progress...") {
		t.Errorf("stdout = %q, want stdout only", res.Stdout)
	}
	if !strings.Contains(res.Tail, "stderr noise") || !strings.Contains(res.Tail, "progress...") {
		t.Errorf("tail = %q, want stdout and stderr combined", res.Tail)
	}
	if got := strings.TrimSpace(read(t, f.path("event"))); got != "status" {
		t.Errorf("MILL_HOOK_EVENT = %q, want status", got)
	}
	if f.stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", f.stderr.String())
	}
	_, d := onlyEvent(t, f.st)
	if d.Event != "status" || d.ExitCode != 0 {
		t.Errorf("event data = %+v", d)
	}
}

func TestStatusUnconfiguredHookIsSkipped(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	res, err := f.runner(hookConfig(), time.Millisecond).Run(context.Background(), EventStatus, "g1", "g1/a", struct{}{})
	if err != nil || !res.Skipped || res.Failed() {
		t.Fatalf("result = %+v, err = %v; want skipped", res, err)
	}
	if evs := hookEvents(t, f.st); len(evs) != 0 {
		t.Errorf("got %d events for a skipped hook", len(evs))
	}
}

func TestStatusNonJSONOutputFailsTheRun(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := hookConfig()
	cfg.Status = "echo 'all good'"

	res, err := f.runner(cfg, 5*time.Millisecond).Run(context.Background(), EventStatus, "g1", "g1/a", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Failed() || res.Status != nil || res.ExitCode != 0 {
		t.Errorf("result = %+v, want failed with exit 0 and no status", res)
	}
	if !strings.Contains(f.stderr.String(), "status hook printed no mill/status@1 JSON: all good") {
		t.Errorf("stderr = %q", f.stderr.String())
	}
	evs := hookEvents(t, f.st)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	var d struct {
		Event    string `json:"event"`
		ExitCode int    `json:"exit_code"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal(evs[0].Data, &d); err != nil {
		t.Fatal(err)
	}
	if d.Event != "status" || d.ExitCode != 0 || d.Error == "" {
		t.Errorf("event data = %+v, want event status, exit 0 and an error", d)
	}
}

func TestStatusFailingHookIsReportedAndRecorded(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := hookConfig()
	cfg.Status = "echo '" + validStatus + "'; echo boom >&2; exit 3"

	res, err := f.runner(cfg, 10*time.Millisecond).Run(context.Background(), EventStatus, "g1", "g1/a", struct{}{})
	if err != nil {
		t.Fatalf("Run returned %v for a failing hook, want nil", err)
	}
	if !res.Failed() || res.ExitCode != 3 || res.Status != nil {
		t.Errorf("result = %+v", res)
	}
	if msg := f.stderr.String(); !strings.Contains(msg, "status hook for g1/a failed (exit 3):") || !strings.Contains(msg, "boom") {
		t.Errorf("stderr = %q", msg)
	}
	_, d := onlyEvent(t, f.st)
	if d.Event != "status" || d.ExitCode != 3 || !strings.Contains(d.OutputTail, "boom") || d.TimedOut || d.DurationMS != 10 {
		t.Errorf("event data = %+v", d)
	}
}

func TestStatusLongSummaryIsCut(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 3000)
	st, err := ParseStatus([]byte(`{"schema":"mill/status@1","state":"failure","checks":[{"name":"ci","state":"failure","summary":"` + long + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Checks[0].Summary; got != long[:2048] {
		t.Errorf("summary is %d bytes, want 2048", len(got))
	}
}

func TestStatusParseTakesLastObject(t *testing.T) {
	t.Parallel()
	in := `{"schema":"mill/status@1","state":"pending","checks":[]}` + "\nnot json\n" + `{"schema":"mill/status@1","state":"success","checks":[]}` + "\n"
	st, err := ParseStatus([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "success" {
		t.Errorf("state = %q, want success (the last object)", st.State)
	}
}

func TestStatusParseRejectsInvalidDocuments(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]string{
		"empty":            "",
		"not json":         "hello",
		"wrong schema":     `{"schema":"mill/status@2","state":"success","checks":[]}`,
		"bad state":        `{"schema":"mill/status@1","state":"ok","checks":[]}`,
		"unnamed check":    `{"schema":"mill/status@1","state":"success","checks":[{"name":"","state":"success","summary":""}]}`,
		"bad check state":  `{"schema":"mill/status@1","state":"success","checks":[{"name":"ci","state":"green","summary":""}]}`,
		"missing state":    `{"schema":"mill/status@1","checks":[]}`,
		"malformed object": `{"schema":"mill/status@1",`,
	} {
		if st, err := ParseStatus([]byte(in)); err == nil {
			t.Errorf("%s: ParseStatus = %+v, want an error", name, st)
		}
	}
}

func TestStatusCheckEventSetsEvent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	out, err := CheckEvent(context.Background(), hookConfig(), f.root, `echo "$MILL_HOOK_EVENT $MILL_HOOK_CHECK"`, EventStatus)
	if err != nil || out != "status 1" {
		t.Errorf("CheckEvent = %q, %v; want \"status 1\"", out, err)
	}
}
