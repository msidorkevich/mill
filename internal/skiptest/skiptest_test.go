package skiptest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/msidorkevich/mill/internal/gitx"
)

// repo creates a temporary git repo whose first commit holds files and returns its dir and sha.
func repo(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	if _, err := gitx.Run(context.Background(), dir, "init", "-q", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	return dir, commit(t, dir, files)
}

func commit(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sha, err := gitx.CommitAll(context.Background(), dir, "change")
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func added(t *testing.T, dir, start, head string) []Skip {
	t.Helper()
	got, err := Added(context.Background(), dir, start, head)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

const base = "package x\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {\n}\n"

func TestSkipWithLiteralReportsFileLineAndReason(t *testing.T) {
	dir, start := repo(t, map[string]string{"pkg/a_test.go": base})
	head := commit(t, dir, map[string]string{"pkg/a_test.go": "package x\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {\n\tt.Skip(\"why\")\n}\n"})
	want := []Skip{{File: "pkg/a_test.go", Line: 6, Reason: "why"}}
	if got := added(t, dir, start, head); !reflect.DeepEqual(got, want) {
		t.Fatalf("skips = %+v, want %+v", got, want)
	}
}

func TestSkipfReportsFormatStringAndAnyReceiver(t *testing.T) {
	dir, start := repo(t, map[string]string{"a_test.go": base})
	head := commit(t, dir, map[string]string{
		"a_test.go": base + "\nfunc TestB(tt *testing.T) {\n\ttt.Skipf(\"flaky on %s\", \"ci\")\n}\n",
		"b_test.go": "package x\n\nimport \"testing\"\n\nfunc BenchmarkC(b *testing.B) {\n\tb.Skip(`raw reason`)\n}\n",
	})
	want := []Skip{
		{File: "a_test.go", Line: 9, Reason: "flaky on %s"},
		{File: "b_test.go", Line: 6, Reason: "raw reason"},
	}
	if got := added(t, dir, start, head); !reflect.DeepEqual(got, want) {
		t.Fatalf("skips = %+v, want %+v", got, want)
	}
}

func TestSkipNowAndNonLiteralSkipReportEmptyReason(t *testing.T) {
	dir, start := repo(t, map[string]string{"a_test.go": base})
	head := commit(t, dir, map[string]string{"a_test.go": base + "\nfunc TestB(t *testing.T) {\n\tt.SkipNow()\n\tt.Skip(reason)\n}\n"})
	want := []Skip{{File: "a_test.go", Line: 9, Reason: ""}, {File: "a_test.go", Line: 10, Reason: ""}}
	if got := added(t, dir, start, head); !reflect.DeepEqual(got, want) {
		t.Fatalf("skips = %+v, want %+v", got, want)
	}
}

func TestSkipPresentBeforeStartIsNotReported(t *testing.T) {
	old := "package x\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {\n\tt.Skip(\"old\")\n}\n"
	dir, start := repo(t, map[string]string{"a_test.go": old})
	head := commit(t, dir, map[string]string{"a_test.go": "// header\n" + old + "\nfunc TestB(t *testing.T) {}\n"})
	if got := added(t, dir, start, head); len(got) != 0 {
		t.Fatalf("skips = %+v, want none", got)
	}
}

func TestSkipInNonTestFileIsIgnored(t *testing.T) {
	dir, start := repo(t, map[string]string{"a.go": "package x\n"})
	head := commit(t, dir, map[string]string{
		"a.go":      "package x\n\nfunc f(t T) { t.Skip(\"no\") }\n",
		"notes.txt": "t.Skip(\"no\")\n",
	})
	if got := added(t, dir, start, head); len(got) != 0 {
		t.Fatalf("skips = %+v, want none", got)
	}
}

func TestSkipDetectionFailsOnUnknownRevision(t *testing.T) {
	dir, start := repo(t, map[string]string{"a_test.go": base})
	if _, err := Added(context.Background(), dir, start, "0000000000000000000000000000000000000000"); err == nil {
		t.Fatal("Added succeeded on an unknown revision")
	}
}
