package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/runlock"
	"github.com/msidorkevich/mill/internal/store"
)

// crashScript is the fan-in script where b's implementer waits bDelayMS before finishing.
func crashScript(bDelayMS string) string {
	return `{"tasks": {
  "g1/a": ` + taskSteps(`{"a.txt": "a\n"}`) + `,
  "g1/b": {"implementer": [{"delay_ms": ` + bDelayMS + `, "output": {"summary": "s", "files_changed": [], "notes": ""}, "files": {"b.txt": "b\n"}}],
           "verifier": [{"output": {"verdict": "pass", "summary": "ok", "findings": []}}]},
  "g1/c": ` + taskSteps(`{"c.txt": "c\n"}`) + `
}}`
}

func TestCrashKilledRunRecoversWithoutDuplicates(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go tool not available")
	}
	bin := filepath.Join(t.TempDir(), "mill")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/msidorkevich/mill/cmd/mill").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	root := goalRepo(t, crashScript("60000"), "")
	paths := project.PathsAt(root)
	ctx := context.Background()

	var firstOut bytes.Buffer
	first := exec.Command(bin, "run", "g1")
	first.Dir = root
	first.Stdout, first.Stderr = &firstOut, &firstOut
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			first.Process.Kill()
			first.Wait()
		}
	}()

	s, err := store.Open(ctx, paths.DB, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	deadline := time.Now().Add(60 * time.Second)
	for {
		atts, err := s.Attempts(ctx, "g1/b")
		if err != nil {
			t.Fatal(err)
		}
		running := false
		for _, a := range atts {
			running = running || a.Status == store.AttemptRunning
		}
		ta, err := s.GetTask(ctx, "g1/a")
		if err != nil {
			t.Fatal(err)
		}
		if running && ta.Status == store.TaskVerified {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("g1/b never had a running attempt; output:\n%s", firstOut.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := first.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	first.Wait()
	waited = true

	lock, err := runlock.Acquire(paths.RunLock)
	if err != nil {
		t.Fatalf("run lock still held right after the killed mill was reaped: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if tk, err := s.GetTask(ctx, "g1/b"); err != nil || tk.Status != store.TaskRunning {
		t.Fatalf("g1/b after kill = %+v, %v; want running", tk, err)
	}

	if err := os.WriteFile(filepath.Join(root, "script.json"), []byte(crashScript("0")), 0o644); err != nil {
		t.Fatal(err)
	}
	second := exec.Command(bin, "run", "g1")
	second.Dir = root
	var out bytes.Buffer
	second.Stdout, second.Stderr = &out, &out
	if err := second.Run(); err != nil {
		t.Fatalf("second run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "recovered") {
		t.Errorf("second run output does not mention recovery:\n%s", out.String())
	}

	for _, id := range []string{"g1/a", "g1/b", "g1/c"} {
		tk, err := s.GetTask(ctx, id)
		if err != nil || tk.Status != store.TaskVerified {
			t.Errorf("%s = %+v, %v; want verified", id, tk, err)
		}
		atts, err := s.Attempts(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, a := range atts {
			key := a.Role + "/" + string(rune('0'+a.N))
			if seen[key] {
				t.Errorf("%s: duplicate attempt number %d for role %s", id, a.N, a.Role)
			}
			seen[key] = true
		}
	}

	var bImpl, aImpl []store.Attempt
	atts, _ := s.Attempts(ctx, "g1/b")
	for _, a := range atts {
		if a.Role == "implementer" {
			bImpl = append(bImpl, a)
		}
	}
	atts, _ = s.Attempts(ctx, "g1/a")
	for _, a := range atts {
		if a.Role == "implementer" {
			aImpl = append(aImpl, a)
		}
	}
	if len(bImpl) != 2 || bImpl[0].Status != store.AttemptAbandoned || bImpl[1].Status == store.AttemptAbandoned ||
		bImpl[1].Status == store.AttemptRunning {
		t.Errorf("g1/b implementer attempts = %+v; want one abandoned then one finished", bImpl)
	}
	if len(aImpl) != 1 {
		t.Errorf("g1/a implementer attempts = %d; want 1", len(aImpl))
	}

	branches, err := gitx.Run(ctx, root, "branch", "--list", "mill/g1/*", "--format=%(refname:short)")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(branches); len(got) != 3 {
		t.Errorf("branches = %v; want exactly 3", got)
	}
	br, err := s.GetBranch(ctx, "g1/c")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, err := os.Stat(filepath.Join(br.Worktree, f)); err != nil {
			t.Errorf("g1/c worktree lacks %s: %v", f, err)
		}
	}
}
