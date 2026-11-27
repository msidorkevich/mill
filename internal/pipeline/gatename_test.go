//go:build unix

package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/host"
	"github.com/msidorkevich/mill/internal/runner"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/testenv"
)

// labelHerdr is a herdr host whose fake binary creates tabs but never runs a command, so
// every gate command falls back to the exec host. It logs each tab label it is asked for
// to the returned file, one per line.
func labelHerdr(t *testing.T) (*host.Herdr, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "labels.log")
	script := "#!/bin/sh\n" +
		"case \"$1 $2\" in\n" +
		"'tab create')\n" +
		"  while [ $# -gt 0 ]; do if [ \"$1\" = --label ]; then echo \"$2\" >> '" + log + "'; fi; shift; done\n" +
		"  echo '{\"result\":{\"root_pane\":{\"pane_id\":\"w1:p1\"},\"tab\":{\"tab_id\":\"w1:t1\"}}}' ;;\n" +
		"'tab close') echo '{\"result\":{\"type\":\"ok\"}}' ;;\n" +
		"esac\nexit 0\n"
	bin := filepath.Join(dir, "herdr")
	testenv.WriteExecutable(t, bin, []byte(script))
	return &host.Herdr{Bin: bin, Poll: time.Millisecond, StartTimeout: 2 * time.Second, Grace: 100 * time.Millisecond}, log
}

// distinctLabels are the labels in log, each once, in the order first asked for.
func distinctLabels(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	seen := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	return out
}

func TestGateNameCarriesTaskAttemptAndCommand(t *testing.T) {
	t.Parallel()
	e := setup(t, "test -f hello.txt", runner.Script{
		runner.RoleImplementer: {impl(map[string]string{"other.txt": "x\n"}), impl(map[string]string{"hello.txt": "hello\n"})},
		runner.RoleVerifier:    {verdict("pass")},
	}, nil)
	h, log := labelHerdr(t)
	e.p.Host = h
	e.task(t, "named", "true")
	out := e.run(t, "named")
	if out.Status != store.TaskVerified || out.Attempts != 2 {
		t.Fatalf("outcome = %+v\n%s", out, e.out)
	}
	want := []string{"mill named a1 gate 1", "mill named a1 gate 2", "mill named a2 gate 1", "mill named a2 gate 2"}
	if got := distinctLabels(t, log); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("herdr tab names = %q, want %q", got, want)
	}
}
