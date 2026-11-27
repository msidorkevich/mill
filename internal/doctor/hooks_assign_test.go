package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/testenv"
)

// assignRepo is an initialized repository whose on_publish is command, with the hook
// script at rel (relative to the repository root) running body.
func assignRepo(t *testing.T, command, rel, body string) string {
	t.Helper()
	dir := gitRepo(t)
	p := initRepo(t, dir, fmt.Sprintf("check_cmd = \"true\"\non_publish = %q\n", command))
	if rel != "" {
		path := filepath.Join(p.Root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		testenv.WriteExecutable(t, path, []byte("#!/bin/sh\n"+body+"\n"))
	}
	return dir
}

func TestHookCommandWithLeadingAssignmentFindsScript(t *testing.T) {
	t.Parallel()
	dir := assignRepo(t, "MILL_GITHUB_DRAFT=0 hooks/github/pr.sh", "hooks/github/pr.sh",
		`[ "$MILL_GITHUB_DRAFT" = 0 ] || { echo "MILL_GITHUB_DRAFT=$MILL_GITHUB_DRAFT"; exit 1; }`)
	r := runHookDoctor(dir)
	if c := byName(r)["on_publish"]; !r.OK || c.Status != OK {
		t.Fatalf("check = %+v, want ok", c)
	}
}

func TestHookCommandWithEnvAndAssignmentsFindsScript(t *testing.T) {
	t.Parallel()
	dir := assignRepo(t, "env A=1 B=2 hooks/x.sh", "hooks/x.sh",
		`[ "$A" = 1 ] && [ "$B" = 2 ] || { echo "A=$A B=$B"; exit 1; }`)
	r := runHookDoctor(dir)
	if c := byName(r)["on_publish"]; !r.OK || c.Status != OK {
		t.Fatalf("check = %+v, want ok", c)
	}
}

func TestPlainHookCommandStillFindsScript(t *testing.T) {
	t.Parallel()
	dir := assignRepo(t, "hooks/x.sh", "hooks/x.sh", "exit 0")
	r := runHookDoctor(dir)
	if c := byName(r)["on_publish"]; !r.OK || c.Status != OK {
		t.Fatalf("check = %+v, want ok", c)
	}
}

func TestInvalidAssignmentNameIsTreatedAsProgram(t *testing.T) {
	t.Parallel()
	dir := assignRepo(t, "1BAD=x prog", "", "")
	r := runHookDoctor(dir)
	c := byName(r)["on_publish"]
	want := "hook on_publish: program 1BAD=x not found; fix the command in .mill/config.toml"
	if r.OK || c.Status != Fail || c.Detail != want {
		t.Fatalf("check = %+v, want fail %q", c, want)
	}
}

func TestAssignmentOnlyCommandFailsWithClearMessage(t *testing.T) {
	t.Parallel()
	for _, command := range []string{"A=1 B=2", "env A=1", "env"} {
		dir := assignRepo(t, command, "", "")
		r := runHookDoctor(dir)
		c := byName(r)["on_publish"]
		if r.OK || c.Status != Fail || !strings.Contains(c.Detail, "has no program after its NAME=value assignments") {
			t.Errorf("%q: check = %+v, want fail naming the missing program", command, c)
		}
	}
}
