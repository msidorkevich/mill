package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/gitx"
)

// syncCount is what countedSync saw: the git processes one sync started, and how often each
// cacheable lookup ran, keyed by lookup, repository and (for remote URLs) remote.
type syncCount struct {
	processes int64
	lookups   map[string]int
}

// traceGit makes every git process this test starts append git's trace2 events to the
// returned file, by pointing HOME at a global config that names it. git reads trace2 settings
// only from global and system config, and gitx keeps HOME while it drops GIT_* variables.
// The test must not be parallel (t.Setenv).
func traceGit(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	events := filepath.Join(home, "trace2.json")
	cfg := "[user]\n\tname = t\n\temail = t@t\n[trace2]\n\teventTarget = " + events + "\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return events
}

// traceEvent is the part of a git trace2 event countedSync reads.
type traceEvent struct {
	Event    string   `json:"event"`
	SID      string   `json:"sid"`
	Argv     []string `json:"argv"`
	Worktree string   `json:"worktree"`
}

// cachedLookups reads the trace2 events file and counts the git commands the lookup cache
// answers (toplevel, common dir, remote URL, identity), keyed by kind, the repository the
// command ran in and, for remote URLs, the remote. Other commands are not counted.
func cachedLookups(t *testing.T, events string) map[string]int {
	t.Helper()
	data, err := os.ReadFile(events)
	if err != nil {
		t.Fatalf("read git trace: %v", err)
	}
	argv := map[string][]string{}
	repo := map[string]string{}
	var order []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var ev traceEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("git trace line %q: %v", line, err)
		}
		switch ev.Event {
		case "start":
			argv[ev.SID] = ev.Argv
			order = append(order, ev.SID)
		case "def_repo":
			repo[ev.SID] = ev.Worktree
		}
	}
	lookups := map[string]int{}
	for _, sid := range order {
		args := argv[sid]
		if len(args) == 0 {
			continue
		}
		dir := repo[sid]
		var k string
		switch a := strings.Join(args[1:], " "); {
		case a == "rev-parse --show-toplevel":
			k = "toplevel " + dir
		case a == "rev-parse --path-format=absolute --git-common-dir":
			k = "commondir " + dir
		case strings.HasPrefix(a, "remote get-url -- "):
			k = "remote " + dir + " " + args[len(args)-1]
		case a == "config user.email":
			k = "identity " + dir
		default:
			continue
		}
		lookups[k]++
	}
	return lookups
}

// countedSync sets up a fresh 2-task published stack whose first task the forge has merged,
// then runs one `mill sync` with ctx, counting the git processes it starts and the cacheable
// lookups among them. It returns the result with temporary paths and commit shas replaced by
// placeholders, so runs on different setups compare equal.
func countedSync(t *testing.T, ctx context.Context, events string) (result, syncCount) {
	t.Helper()
	root, bare := chainPublished(t, true)
	f := forge(t, bare)
	gitC(t, f, "fetch", "-q", "origin")
	gitC(t, f, "merge", "-q", "--no-ff", "-m", "Merge review/g2/a", "origin/review/g2/a")
	gitC(t, f, "push", "-q", "origin", "main")
	if err := os.Truncate(events, 0); err != nil { // count only the sync
		t.Fatal(err)
	}

	var n atomic.Int64
	var out, errb bytes.Buffer
	env := Env{Stdout: &out, Stderr: &errb, Dir: root, SchedulerPoll: groupBPoll, TaskRunPoll: groupBPoll}
	code := Run(gitx.WithCounter(ctx, &n), env, []string{"sync"})
	r := result{code, normalizeRun(t, out.String()), normalizeRun(t, errb.String())}
	return r, syncCount{n.Load(), cachedLookups(t, events)}
}

var (
	shaPattern     = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
	worktreePrefix = regexp.MustCompile(`<tmp>/\d+-<sha>`) // <worktree_root>/<repo dir>-<hash>
)

// normalizeRun replaces this test's temporary directories and commit shas in s.
func normalizeRun(t *testing.T, s string) string {
	t.Helper()
	base := filepath.Dir(t.TempDir())
	for _, b := range []string{base, resolved(t, base)} {
		s = regexp.MustCompile(regexp.QuoteMeta(b)+`/\d+`).ReplaceAllString(s, "<tmp>")
	}
	s = shaPattern.ReplaceAllString(s, "<sha>")
	return worktreePrefix.ReplaceAllString(s, "<worktrees>")
}

func resolved(t *testing.T, dir string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSyncLookupCacheStartsFewerGitProcessesWithSameOutcome(t *testing.T) {
	events := traceGit(t) // not parallel: it sets HOME
	uncached, nUncached := countedSync(t, gitx.WithoutCache(context.Background()), events)
	cached, nCached := countedSync(t, context.Background(), events)

	if uncached.code != ExitOK {
		t.Fatalf("uncached sync: %+v", uncached)
	}
	if cached.code != uncached.code || cached.stdout != uncached.stdout || cached.stderr != uncached.stderr {
		t.Errorf("cached sync differs from uncached:\ncached:   %+v\nuncached: %+v", cached, uncached)
	}
	if !strings.Contains(cached.stdout, "g2/a: landed") || !strings.Contains(cached.stdout, "Republished:") {
		t.Errorf("sync did not land g2/a and republish g2/b:\n%s", cached.stdout)
	}
	t.Logf("git processes: %d uncached, %d cached", nUncached.processes, nCached.processes)
	t.Logf("cacheable lookups uncached: %v", nUncached.lookups)
	t.Logf("cacheable lookups cached:   %v", nCached.lookups)

	// With the cache, each lookup it answers runs at most once per directory and remote.
	for _, kind := range []string{"toplevel ", "commondir ", "remote ", "identity "} {
		seen := false
		for k := range nCached.lookups {
			seen = seen || strings.HasPrefix(k, kind)
		}
		if !seen {
			t.Errorf("cached sync ran no %slookup; the trace is not seeing it", kind)
		}
	}
	for k, c := range nCached.lookups {
		if c > 1 {
			t.Errorf("cached sync ran %q %d times, want at most once", k, c)
		}
	}
	// The cache answers the repeated remote URL, common dir and identity lookups, a few of
	// ~49 processes. The rest read or change refs, heads, status, diffs and trees, whose
	// answers change during the sync, so they are never cached: the gain only has to be real,
	// not a fixed percentage.
	if nCached.processes >= nUncached.processes {
		t.Errorf("cached sync started %d git processes, uncached %d: want fewer", nCached.processes, nUncached.processes)
	}
}

func TestLookupCacheDoesNotServeChangedRemoteToNextCommand(t *testing.T) {
	t.Parallel()
	root, bare := chainPublished(t, true)
	if r := groupBRun(t, root, "sync"); r.code != ExitOK {
		t.Fatalf("first sync: %+v", r)
	}

	// The remote is gone: the next command's remote check must see that rather than the URL
	// the first command looked up.
	gitC(t, root, "remote", "remove", "origin")
	if r := groupBRun(t, root, "sync"); r.code == ExitOK || !strings.Contains(r.stderr, `remote "origin" is not configured`) {
		t.Fatalf("sync after the remote was removed: %+v", r)
	}

	// A remote added back is used again.
	gitC(t, root, "remote", "add", "origin", bare)
	if r := groupBRun(t, root, "sync"); r.code != ExitOK {
		t.Fatalf("sync after the remote came back: %+v", r)
	}

	// Its URL changes: the next command uses the new one.
	gitC(t, root, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	if r := groupBRun(t, root, "sync"); r.code == ExitOK || !strings.Contains(r.stderr, "gone.git") {
		t.Fatalf("sync after the remote URL changed: %+v", r)
	}
}

func TestKeepWatchPassGetsFreshLookupCache(t *testing.T) {
	t.Parallel()
	root := keepRepo(t, "true")
	passes := 0
	wait := func(ctx context.Context, d time.Duration) bool {
		passes++
		if passes == 1 {
			// Between passes the remote goes away: the second pass must look it up again
			// rather than reuse the URL the first pass found.
			gitC(t, root, "remote", "remove", "origin")
			return true
		}
		return false
	}

	var out, errb bytes.Buffer
	code := Run(context.Background(), Env{Stdout: &out, Stderr: &errb, Dir: root, KeepWait: wait}, []string{"keep", "--watch", "--interval", "10s"})
	if passes != 1 {
		t.Fatalf("waited %d times, want 1 (the second pass should fail)\nstdout:\n%s\nstderr:\n%s", passes, out.String(), errb.String())
	}
	if code != ExitError || !strings.Contains(errb.String(), `remote "origin" is not configured`) {
		t.Fatalf("second pass after the remote was removed: code %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errb.String())
	}
}
