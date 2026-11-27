package publish

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// hookOrderPublish publishes with one shared writer for Out and returns the result and
// everything Publish wrote.
func hookOrderPublish(t *testing.T, o Options) (Result, string) {
	t.Helper()
	var shared bytes.Buffer
	o.Out = &shared
	res, err := Publish(context.Background(), o)
	if err != nil {
		t.Fatalf("Publish = %v, want nil", err)
	}
	return res, shared.String()
}

func TestHookLineAfterPublishedSuccessIsReportedNotWritten(t *testing.T) {
	t.Parallel()
	o, _, _ := fixture(t)
	o.Cfg.OnPublish = hookScript(t, `echo https://example.test/pr/1`)
	res, written := hookOrderPublish(t, o)
	if strings.Contains(written, "hook") {
		t.Errorf("Publish wrote a hook line itself:\n%s", written)
	}
	want := []HookLine{{Line: "hook on_publish for t1: https://example.test/pr/1"}}
	if len(res.HookReport) != 1 || res.HookReport[0] != want[0] {
		t.Errorf("HookReport = %+v, want %+v", res.HookReport, want)
	}
	if n := hookRuns(t, o.Store, "t1"); n != 1 {
		t.Errorf("%d hook-run events, want 1", n)
	}

	var shared bytes.Buffer
	shared.WriteString("Published t1\n")
	PrintHookReport(&shared, &shared, res)
	if got := shared.String(); got != "Published t1\nhook on_publish for t1: https://example.test/pr/1\n" {
		t.Errorf("printed = %q", got)
	}
}

func TestHookLineAfterPublishedFailureIsReportedForStderr(t *testing.T) {
	t.Parallel()
	o, _, _ := fixture(t)
	o.Cfg.OnPublish = hookScript(t, "echo https://example.test/pr/2\nexit 3")
	res, written := hookOrderPublish(t, o)
	if strings.Contains(written, "hook") {
		t.Errorf("Publish wrote a hook line itself:\n%s", written)
	}
	if len(res.HookReport) == 0 {
		t.Fatal("HookReport is empty for a failed hook")
	}
	for _, l := range res.HookReport {
		if !l.Failed {
			t.Errorf("report line %q is not marked failed", l.Line)
		}
	}
	if n := hookRuns(t, o.Store, "t1"); n != 1 {
		t.Errorf("%d hook-run events, want 1", n)
	}

	var out, stderr bytes.Buffer
	PrintHookReport(&out, &stderr, res)
	if out.Len() != 0 {
		t.Errorf("out = %q, want nothing for a failed hook", out.String())
	}
	if !strings.Contains(stderr.String(), "on_publish") {
		t.Errorf("stderr = %q, want the failure report", stderr.String())
	}
}
