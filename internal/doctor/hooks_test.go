package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hookRepo is an initialized repository with config cfgText and an executable
// hooks/hook.sh running script.
func hookRepo(t *testing.T, cfgText, script string) string {
	t.Helper()
	dir := gitRepo(t)
	p := initRepo(t, dir, "check_cmd = \"true\"\n"+cfgText)
	hooks := filepath.Join(p.Root, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "hook.sh"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runHookDoctor(dir string) Report {
	return Run(context.Background(), fakeEnv{tools: goodTools}.opts(dir))
}

func TestUnconfiguredHooksAreSkippedAndDoctorPasses(t *testing.T) {
	t.Parallel()
	r := runHookDoctor(hookRepo(t, "", "exit 1"))
	c := byName(r)
	if !r.OK || c["on_publish"].Status != Skip || c["on_update"].Status != Skip {
		t.Fatalf("report = %+v, want OK with both hooks skipped", r)
	}
}

func TestHookExitingZeroIsOK(t *testing.T) {
	t.Parallel()
	r := runHookDoctor(hookRepo(t, "on_publish = \"./hooks/hook.sh\"\non_update = \"hooks/hook.sh arg\"\n", "exit 0"))
	c := byName(r)
	if !r.OK || c["on_publish"].Status != OK || c["on_update"].Status != OK {
		t.Fatalf("report = %+v, want both hooks ok", r)
	}
}

func TestFailingHookFailsDoctorWithOutputTail(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"on_publish", "on_update"} {
		r := runHookDoctor(hookRepo(t, name+" = \"./hooks/hook.sh\"\n", "echo webhook unreachable >&2\nexit 1"))
		c := byName(r)[name]
		if r.OK || c.Status != Fail {
			t.Fatalf("%s: report = %+v, want failure", name, r)
		}
		for _, want := range []string{"hook " + name, "exit 1", "webhook unreachable"} {
			if !strings.Contains(c.Detail, want) {
				t.Errorf("%s detail %q lacks %q", name, c.Detail, want)
			}
		}
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`"name":"` + name + `","status":"fail"`, "webhook unreachable", `"ok":false`} {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s JSON %s lacks %q", name, b, want)
			}
		}
	}
}

func TestMissingHookProgramFailsWithFix(t *testing.T) {
	t.Parallel()
	// plain.txt exists but is not executable; hooks is a directory.
	for _, program := range []string{"no-such-program", "./hooks/missing.sh", "./plain.txt", "./hooks"} {
		dir := hookRepo(t, fmt.Sprintf("on_publish = %q\n", program+" --flag"), "exit 0")
		if err := os.WriteFile(filepath.Join(dir, "plain.txt"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		r := runHookDoctor(dir)
		c := byName(r)["on_publish"]
		want := "hook on_publish: program " + program + " not found; fix the command in .mill/config.toml"
		if r.OK || c.Status != Fail || c.Detail != want {
			t.Errorf("%s: check = %+v, want fail %q", program, c, want)
		}
	}
}

func TestHookCheckSeesCheckVariableAndEmptyStdin(t *testing.T) {
	t.Parallel()
	script := `[ "$MILL_HOOK_CHECK" = 1 ] || { echo "MILL_HOOK_CHECK=$MILL_HOOK_CHECK"; exit 1; }
in=$(cat)
[ -z "$in" ] || { echo "stdin=$in"; exit 1; }`
	r := runHookDoctor(hookRepo(t, "on_publish = \"./hooks/hook.sh\"\n", script))
	if c := byName(r)["on_publish"]; !r.OK || c.Status != OK {
		t.Fatalf("check = %+v, want ok", c)
	}
}
