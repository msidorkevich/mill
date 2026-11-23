package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/project"
)

// useHostKind sets host.kind in the repo's config and makes sure mill looks like it runs
// outside herdr.
func useHostKind(t *testing.T, root, kind string) {
	t.Helper()
	t.Setenv("HERDR_ENV", "")
	p := project.PathsAt(root)
	b, err := os.ReadFile(p.Config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Config, append(b, []byte("[host]\nkind = \""+kind+"\"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTaskRunFailsWhenHerdrHostIsUnavailable(t *testing.T) {
	root, tf := taskRepo(t, passScript, 3)
	useHostKind(t, root, "herdr")
	r := run(t, root, nil, "task", "run", "--file", tf)
	if r.code != ExitError || !strings.Contains(r.stderr, "mill task run: host.kind = herdr needs mill to run inside a herdr pane") ||
		!strings.Contains(r.stderr, `"auto" or "exec"`) {
		t.Fatalf("task run: %+v", r)
	}
	if strings.Contains(r.stdout, "implementer") {
		t.Fatalf("work started despite the host error:\n%s", r.stdout)
	}
}

func TestTaskRunHostFlagOverridesConfig(t *testing.T) {
	root, tf := taskRepo(t, passScript, 3)
	useHostKind(t, root, "herdr")
	r := run(t, root, nil, "task", "run", "--file", tf, "--host", "exec")
	if r.code != ExitOK || !strings.Contains(r.stdout, "hello: verified") {
		t.Fatalf("task run --host exec: %+v", r)
	}
}

func TestRunFailsWhenHerdrHostIsUnavailableAndHostFlagOverrides(t *testing.T) {
	root, _ := taskRepo(t, passScript, 3)
	useHostKind(t, root, "herdr")
	r := run(t, root, nil, "run", "g")
	if r.code != ExitError || !strings.Contains(r.stderr, "mill run: host.kind = herdr needs mill to run inside a herdr pane") {
		t.Fatalf("run: %+v", r)
	}
	// With --host exec the host is accepted; the run then fails on the missing goal.
	r = run(t, root, nil, "run", "g", "--host", "exec")
	if strings.Contains(r.stderr, "herdr") {
		t.Fatalf("run --host exec still complains about herdr: %+v", r)
	}
}

func TestInvalidHostFlagIsUsageError(t *testing.T) {
	root, tf := taskRepo(t, passScript, 3)
	if r := run(t, root, nil, "task", "run", "--file", tf, "--host", "bogus"); r.code != ExitUsage || !strings.Contains(r.stderr, "--host must be") {
		t.Fatalf("task run: %+v", r)
	}
	if r := run(t, root, nil, "run", "g", "--host", "bogus"); r.code != ExitUsage || !strings.Contains(r.stderr, "--host must be") {
		t.Fatalf("run: %+v", r)
	}
}

func TestTaskRunInsideHerdrUsesHerdrFromPath(t *testing.T) {
	root, tf := taskRepo(t, passScript, 3)
	log := fakeHerdrOnPath(t)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_WORKSPACE_ID", "w8")
	r := run(t, root, nil, "task", "run", "--file", tf)
	if r.code != ExitOK || !strings.Contains(r.stdout, "hello: verified") {
		t.Fatalf("task run: %+v", r)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the auto host never called the fake herdr: %v", err)
	}
	calls := string(b)
	if !strings.Contains(calls, "tab create --workspace w8 --label mill ") || !strings.Contains(calls, "tab close w8:t2") {
		t.Fatalf("herdr calls:\n%s", calls)
	}
}
