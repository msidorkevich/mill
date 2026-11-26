package doctor_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/cli"
	"github.com/msidorkevich/mill/internal/doctor"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/project"
)

func TestLegacySizesWarnsOnlyBesideModels(t *testing.T) {
	t.Parallel()
	const models = "[models]\nS = { medium = \"sonnet\" }\n"
	const sizes = "[sizes.S]\nmodel = \"sonnet\"\n[sizes.M]\nmodel = \"opus\"\n"
	for _, tc := range []struct {
		name, cfg string
		warn      bool
	}{
		{"both", models + sizes, true},
		{"models only", models, false},
		{"sizes only", sizes, false},
		{"neither", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := exec.LookPath(gitx.Bin()); err != nil {
				t.Skip("git not installed")
			}
			dir := t.TempDir()
			if out, err := exec.Command(gitx.Bin(), "init", "-q", dir).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v: %s", err, out)
			}
			// Simulated tools: doctor never runs the machine's claude or herdr.
			tools := &doctor.Options{
				GitBin:   func() string { return "git" },
				LookPath: func(name string) (string, error) { return "/fake/" + name, nil },
				Output:   func(string, ...string) (string, error) { return "git version 2.50.1", nil },
				Getenv:   func(string) string { return "" },
			}
			mill := func(args ...string) (int, string) {
				var out bytes.Buffer
				code := cli.Run(context.Background(), cli.Env{Stdout: &out, Stderr: &out, Dir: dir, Doctor: tools}, args)
				return code, out.String()
			}
			if code, out := mill("init"); code != cli.ExitOK {
				t.Fatalf("init: %d\n%s", code, out)
			}
			root, err := project.GitRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(project.PathsAt(root).Config, []byte("check_cmd = \"true\"\n"+tc.cfg), 0o644); err != nil {
				t.Fatal(err)
			}

			code, out := mill("doctor")
			if code != cli.ExitOK {
				t.Fatalf("doctor: %d, want success\n%s", code, out)
			}
			var line string
			for _, l := range strings.Split(out, "\n") {
				if strings.Contains(l, "[sizes") {
					line = l
				}
			}
			if !tc.warn {
				if line != "" {
					t.Errorf("unexpected [sizes] line %q", line)
				}
				return
			}
			for _, want := range []string{"warn", "[sizes] is ignored", "delete [sizes.S], [sizes.M]"} {
				if !strings.Contains(line, want) {
					t.Errorf("[sizes] line %q lacks %q\n%s", line, want, out)
				}
			}
		})
	}
}
