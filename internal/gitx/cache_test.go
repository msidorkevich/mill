package gitx

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// countedCtx returns a context carrying c (nil for none) and a git invocation counter.
func countedCtx(c *Cache) (context.Context, *atomic.Int64) {
	var n atomic.Int64
	ctx := WithCounter(context.Background(), &n)
	if c != nil {
		ctx = WithCache(ctx, c)
	}
	return ctx, &n
}

// lookupAll runs every cached lookup once and fails the test on any error.
func lookupAll(t *testing.T, ctx context.Context, dir string) {
	t.Helper()
	if _, err := TopLevel(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := CommonDir(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoteURL(ctx, dir, "origin"); err != nil {
		t.Fatal(err)
	}
	if _, err := hasIdentity(ctx, dir); err != nil {
		t.Fatal(err)
	}
}

func TestCachedLookupStartsNoGitTheSecondTime(t *testing.T) {
	t.Parallel()
	r := repo(t)
	mustRun(t, context.Background(), r, "remote", "add", "origin", "https://example.invalid/a.git")
	ctx, n := countedCtx(NewCache())

	lookupAll(t, ctx, r)
	if got := n.Load(); got != 4 {
		t.Fatalf("first lookups started %d git processes, want 4", got)
	}
	lookupAll(t, ctx, r)
	// The same dir written differently is the same key.
	lookupAll(t, ctx, filepath.Join(r, "."))
	if got := n.Load(); got != 4 {
		t.Fatalf("cached lookups started %d extra git processes, want 0", got-4)
	}

	top, err := TopLevel(ctx, r)
	want, _ := filepath.EvalSymlinks(r)
	if err != nil || top != want {
		t.Fatalf("TopLevel = %q, %v; want %q", top, err, want)
	}
	url, err := RemoteURL(ctx, r, "origin")
	if err != nil || url != "https://example.invalid/a.git" {
		t.Fatalf("RemoteURL = %q, %v", url, err)
	}
	common, err := CommonDir(ctx, r)
	if err != nil || filepath.Base(common) != ".git" {
		t.Fatalf("CommonDir = %q, %v", common, err)
	}
}

func TestLookupWithoutCacheRunsGitEveryTime(t *testing.T) {
	t.Parallel()
	r := repo(t)
	mustRun(t, context.Background(), r, "remote", "add", "origin", "https://example.invalid/a.git")

	ctx, n := countedCtx(nil)
	lookupAll(t, ctx, r)
	lookupAll(t, ctx, r)
	if got := n.Load(); got != 8 {
		t.Fatalf("lookups without a cache started %d git processes, want 8", got)
	}

	// WithoutCache wins over a cache attached before or after it.
	var m atomic.Int64
	ctx = WithCounter(WithCache(WithoutCache(WithCache(context.Background(), NewCache())), NewCache()), &m)
	if CacheFrom(ctx) != nil {
		t.Fatal("CacheFrom returned a cache on an opted-out context")
	}
	lookupAll(t, ctx, r)
	lookupAll(t, ctx, r)
	if got := m.Load(); got != 8 {
		t.Fatalf("opted-out lookups started %d git processes, want 8", got)
	}
}

func TestLookupFailuresAreNotCached(t *testing.T) {
	t.Parallel()
	r := repo(t)
	notRepo := t.TempDir()
	ctx, n := countedCtx(NewCache())

	for i := 0; i < 2; i++ {
		_, err := TopLevel(ctx, notRepo)
		var nre *NotRepositoryError
		if !errors.As(err, &nre) {
			t.Fatalf("TopLevel outside a repository = %v, want a *NotRepositoryError", err)
		}
		if _, err := CommonDir(ctx, notRepo); err == nil {
			t.Fatal("CommonDir outside a repository succeeded")
		}
		if _, err := RemoteURL(ctx, r, "nope"); err == nil {
			t.Fatal("RemoteURL of a missing remote succeeded")
		}
	}
	if got := n.Load(); got != 6 {
		t.Fatalf("failing lookups started %d git processes, want 6 (each retried)", got)
	}

	// Once the remote exists, the lookup sees it.
	mustRun(t, context.Background(), r, "remote", "add", "nope", "https://example.invalid/n.git")
	if url, err := RemoteURL(ctx, r, "nope"); err != nil || url != "https://example.invalid/n.git" {
		t.Fatalf("RemoteURL after adding the remote = %q, %v", url, err)
	}
}

func TestRemoteChangedAfterFailedCommandIsReread(t *testing.T) {
	t.Parallel()
	r := repo(t)
	bg := context.Background()
	mustRun(t, bg, r, "remote", "add", "origin", "https://example.invalid/old.git")
	mustRun(t, bg, r, "config", "user.email", "a@example.invalid")
	ctx, n := countedCtx(NewCache())

	if url, err := RemoteURL(ctx, r, "origin"); err != nil || url != "https://example.invalid/old.git" {
		t.Fatalf("RemoteURL = %q, %v", url, err)
	}
	if ok, err := hasIdentity(ctx, r); err != nil || !ok {
		t.Fatalf("hasIdentity = %v, %v", ok, err)
	}
	mustRun(t, bg, r, "remote", "set-url", "origin", "https://example.invalid/new.git")
	mustRun(t, bg, r, "config", "user.email", "")

	// Still cached: nothing failed in r yet.
	before := n.Load()
	if url, _ := RemoteURL(ctx, r, "origin"); url != "https://example.invalid/old.git" {
		t.Fatalf("RemoteURL before any failure = %q, want the cached old URL", url)
	}
	if n.Load() != before {
		t.Fatal("cached RemoteURL started git")
	}

	// A failure in another dir drops nothing for r.
	other := repo(t)
	if _, err := Run(ctx, other, "rev-parse", "--verify", "no-such-ref"); err == nil {
		t.Fatal("rev-parse of a missing ref succeeded")
	}
	if url, _ := RemoteURL(ctx, r, "origin"); url != "https://example.invalid/old.git" {
		t.Fatalf("RemoteURL after a failure elsewhere = %q, want the cached old URL", url)
	}

	// Each failing runner drops r's remote and identity entries.
	fails := map[string]func() error{
		"Run": func() error {
			_, err := Run(ctx, r, "rev-parse", "--verify", "no-such-ref")
			return err
		},
		"runExit": func() error {
			_, _, err := runExit(ctx, r, "cat-file", "-t", "no-such-ref")
			return err
		},
		"IsAncestor": func() error {
			_, err := IsAncestor(ctx, r, "no-such-ref", "HEAD")
			return err
		},
	}
	for name, fail := range fails {
		mustRun(t, bg, r, "remote", "set-url", "origin", "https://example.invalid/"+name+".git")
		if err := fail(); err == nil {
			t.Fatalf("%s: the failing command succeeded", name)
		}
		if url, err := RemoteURL(ctx, r, "origin"); err != nil || url != "https://example.invalid/"+name+".git" {
			t.Fatalf("%s: RemoteURL after a failed command = %q, %v; want the new URL", name, url, err)
		}
		if ok, err := hasIdentity(ctx, r); err != nil || ok {
			t.Fatalf("%s: hasIdentity after a failed command = %v, %v; want the re-read false", name, ok, err)
		}
	}
}

func TestIdentityAnswerIsPerCache(t *testing.T) {
	t.Parallel()
	r := repo(t)
	bg := context.Background()
	mustRun(t, bg, r, "config", "user.email", "a@example.invalid")

	first, n := countedCtx(NewCache())
	if ok, err := hasIdentity(first, r); err != nil || !ok {
		t.Fatalf("hasIdentity = %v, %v; want true", ok, err)
	}
	mustRun(t, bg, r, "config", "user.email", "")

	second, _ := countedCtx(NewCache())
	if ok, err := hasIdentity(second, r); err != nil || ok {
		t.Fatalf("hasIdentity in a new cache = %v, %v; want false", ok, err)
	}
	if ok, _ := hasIdentity(first, r); !ok {
		t.Fatal("the first cache lost its answer")
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("the first cache started %d git processes, want 1", got)
	}

	// The cached "no identity" answer still makes commits carry mill's identity, with one
	// identity lookup for several commits.
	ctx, m := countedCtx(NewCache())
	if _, err := CommitTree(ctx, r, mustRun(t, bg, r, "rev-parse", "HEAD^{tree}"), "", "one"); err != nil {
		t.Fatal(err)
	}
	afterFirst := m.Load()
	sha, err := CommitTree(ctx, r, mustRun(t, bg, r, "rev-parse", "HEAD^{tree}"), "", "two")
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Load() - afterFirst; got != 1 {
		t.Fatalf("the second commit started %d git processes, want 1 (no identity lookup)", got)
	}
	if email := mustRun(t, bg, r, "show", "-s", "--format=%ae", sha); email != "mill@localhost" {
		t.Fatalf("commit author email = %q, want mill@localhost", email)
	}
}

func TestCacheConcurrentMixedLookups(t *testing.T) {
	t.Parallel()
	r := repo(t)
	bg := context.Background()
	mustRun(t, bg, r, "remote", "add", "origin", "https://example.invalid/a.git")
	wt := filepath.Join(t.TempDir(), "wt")
	if err := AddWorktree(bg, r, wt, "side", mustRun(t, bg, r, "rev-parse", "HEAD")); err != nil {
		t.Fatal(err)
	}
	wantTop := map[string]string{}
	for _, d := range []string{r, wt} {
		top, err := TopLevel(bg, d)
		if err != nil {
			t.Fatal(err)
		}
		wantTop[d] = top
	}
	wantCommon, err := CommonDir(bg, r)
	if err != nil {
		t.Fatal(err)
	}

	ctx := WithCache(bg, NewCache())
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d := []string{r, wt}[i%2]
			switch i % 5 {
			case 0:
				if top, err := TopLevel(ctx, d); err != nil || top != wantTop[d] {
					errs <- fmt.Errorf("TopLevel(%s) = %q, %v", d, top, err)
				}
			case 1:
				if c, err := CommonDir(ctx, d); err != nil || c != wantCommon {
					errs <- fmt.Errorf("CommonDir(%s) = %q, %v", d, c, err)
				}
			case 2:
				if u, err := RemoteURL(ctx, d, "origin"); err != nil || u != "https://example.invalid/a.git" {
					errs <- fmt.Errorf("RemoteURL(%s) = %q, %v", d, u, err)
				}
			case 3:
				if _, err := hasIdentity(ctx, d); err != nil {
					errs <- fmt.Errorf("hasIdentity(%s): %v", d, err)
				}
			case 4:
				// A failing command invalidates concurrently with the lookups.
				if _, err := Run(ctx, d, "rev-parse", "--verify", "no-such-ref"); err == nil {
					errs <- fmt.Errorf("rev-parse of a missing ref succeeded in %s", d)
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
