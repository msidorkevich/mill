package publish

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/msidorkevich/mill/internal/testenv"
)

// hookScript writes a shell script that prints body and returns the command that runs it.
func hookScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hook.sh")
	testenv.WriteExecutable(t, path, []byte("#!/bin/sh\n"+body+"\n"))
	return "sh '" + path + "'"
}

func TestHookOutputSuccessPrintsLastNonEmptyLine(t *testing.T) {
	t.Parallel()
	o, _, _ := fixture(t)
	o.Cfg.OnPublish = hookScript(t, `printf 'creating\nhttps://example.test/pr/1\n\n'`)
	var out, stderr bytes.Buffer
	o.Out = &out

	res, err := Publish(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	PrintHookReport(&out, &stderr, res)
	want := "hook on_publish for t1: https://example.test/pr/1\n"
	if out.String() != want {
		t.Errorf("Out = %q, want %q", out.String(), want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", stderr.String())
	}
}

func TestHookOutputEmptyOrBlankOutputPrintsNoLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body string
	}{
		{name: "no output", body: "true"},
		{name: "whitespace only", body: `printf '  \n\t\n\n'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o, _, _ := fixture(t)
			o.Cfg.OnPublish = hookScript(t, tc.body)
			var out bytes.Buffer
			o.Out = &out

			res, err := Publish(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			PrintHookReport(&out, &bytes.Buffer{}, res)
			if out.Len() != 0 {
				t.Errorf("Out = %q, want nothing", out.String())
			}
			if n := hookRuns(t, o.Store, "t1"); n != 1 {
				t.Errorf("%d hook-run events, want 1", n)
			}
		})
	}
}

func TestHookOutputFailedHookPrintsNoSuccessLine(t *testing.T) {
	t.Parallel()
	o, _, _ := fixture(t)
	o.Cfg.OnPublish = hookScript(t, "echo https://example.test/pr/2\nexit 3")
	var out, stderr bytes.Buffer
	o.Out = &out

	res, err := Publish(context.Background(), o)
	if err != nil {
		t.Fatalf("Publish with a failing hook = %v, want nil", err)
	}
	PrintHookReport(&out, &stderr, res)
	if out.Len() != 0 {
		t.Errorf("Out = %q, want nothing for a failed hook", out.String())
	}
	if !bytes.HasPrefix(stderr.Bytes(), []byte("hook on_publish for t1 failed (exit 3)")) {
		t.Errorf("stderr = %q, want the failure report", stderr.String())
	}
}
