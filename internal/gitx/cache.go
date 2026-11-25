package gitx

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// Cache remembers the answers of repeated git lookups (TopLevel, CommonDir, RemoteURL and
// whether a repository has an identity) for as long as a context carries it. Only successful
// answers are cached. It is safe for concurrent use.
type Cache struct {
	mu   sync.Mutex
	vals map[cacheKey]string
	gens map[string]uint64 // dir -> invalidation generation
}

type cacheKey struct {
	kind   string // "toplevel", "commondir", "remote" or "identity"
	dir    string // absolute, cleaned
	remote string // only for "remote"
}

// NewCache returns an empty cache.
func NewCache() *Cache {
	return &Cache{vals: map[cacheKey]string{}, gens: map[string]uint64{}}
}

type cacheCtxKey struct{}

type counterCtxKey struct{}

// cacheSlot is the context value of cacheCtxKey: a cache, or the opt-out marker.
type cacheSlot struct {
	c   *Cache
	off bool
}

// WithCache returns ctx carrying c, so gitx lookups made with it reuse earlier answers. A ctx
// marked by WithoutCache is returned unchanged: the opt-out wins.
func WithCache(ctx context.Context, c *Cache) context.Context {
	if s, ok := ctx.Value(cacheCtxKey{}).(cacheSlot); ok && s.off {
		return ctx
	}
	return context.WithValue(ctx, cacheCtxKey{}, cacheSlot{c: c})
}

// WithoutCache returns ctx marked so that no cache applies: lookups run git every time, and
// WithCache on it (or on contexts derived from it) attaches nothing.
func WithoutCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, cacheCtxKey{}, cacheSlot{off: true})
}

// CacheFrom returns the cache ctx carries, or nil when it has none or is marked by
// WithoutCache.
func CacheFrom(ctx context.Context) *Cache {
	s, _ := ctx.Value(cacheCtxKey{}).(cacheSlot)
	return s.c
}

// WithCounter returns ctx carrying n, which gitx increments each time it starts a git process
// with that ctx (or one derived from it). Tests use it to count git invocations.
func WithCounter(ctx context.Context, n *atomic.Int64) context.Context {
	return context.WithValue(ctx, counterCtxKey{}, n)
}

// started records that a git process is starting with ctx.
func started(ctx context.Context) {
	if n, _ := ctx.Value(counterCtxKey{}).(*atomic.Int64); n != nil {
		n.Add(1)
	}
}

// failed records that a git command run with ctx failed in dir: the remote URLs and the
// identity answer cached for dir may be stale, so they are dropped.
func failed(ctx context.Context, dir string) {
	if c := CacheFrom(ctx); c != nil {
		c.dropDir(cacheDir(dir))
	}
}

func cacheDir(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return filepath.Clean(dir)
}

func (c *Cache) get(k cacheKey) (string, uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.vals[k]
	return v, c.gens[k.dir], ok
}

// put stores v unless dir's entries were invalidated since gen was read.
func (c *Cache) put(k cacheKey, gen uint64, v string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if (k.kind == "remote" || k.kind == "identity") && c.gens[k.dir] != gen {
		return
	}
	c.vals[k] = v
}

func (c *Cache) dropDir(dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gens[dir]++
	for k := range c.vals {
		if k.dir == dir && (k.kind == "remote" || k.kind == "identity") {
			delete(c.vals, k)
		}
	}
}

// lookup returns the cached answer for k or computes it with fn, caching it on success.
func lookup(ctx context.Context, k cacheKey, fn func() (string, error)) (string, error) {
	c := CacheFrom(ctx)
	if c == nil {
		return fn()
	}
	v, gen, ok := c.get(k)
	if ok {
		return v, nil
	}
	v, err := fn()
	if err != nil {
		return "", err
	}
	c.put(k, gen, v)
	return v, nil
}

// NotRepositoryError reports that a directory is not inside a git work tree. Err is git's
// error, including its stderr.
type NotRepositoryError struct {
	Dir string
	Err error
}

func (e *NotRepositoryError) Error() string { return e.Err.Error() }

func (e *NotRepositoryError) Unwrap() error { return e.Err }

// TopLevel returns the root of the work tree dir belongs to (git rev-parse --show-toplevel),
// with symlinks resolved. When dir is not in a git work tree the error is a
// *NotRepositoryError. Answers are cached when ctx carries a Cache.
func TopLevel(ctx context.Context, dir string) (string, error) {
	return lookup(ctx, cacheKey{kind: "toplevel", dir: cacheDir(dir)}, func() (string, error) {
		out, err := Run(ctx, dir, "rev-parse", "--show-toplevel")
		if err != nil {
			if strings.Contains(err.Error(), "not a git repository") {
				return "", &NotRepositoryError{Dir: dir, Err: err}
			}
			return "", err
		}
		if out == "" {
			return "", fmt.Errorf("git rev-parse --show-toplevel in %s printed nothing; is it a bare repository?", dir)
		}
		resolved, err := filepath.EvalSymlinks(out)
		if err != nil {
			return "", fmt.Errorf("resolve the work tree root %s: %w", out, err)
		}
		return resolved, nil
	})
}

// CommonDir returns the absolute path of the git directory shared by all worktrees of the
// repository dir belongs to (git rev-parse --git-common-dir). Answers are cached when ctx
// carries a Cache.
func CommonDir(ctx context.Context, dir string) (string, error) {
	return lookup(ctx, cacheKey{kind: "commondir", dir: cacheDir(dir)}, func() (string, error) {
		out, err := Run(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err == nil && out == "" {
			err = fmt.Errorf("git rev-parse --git-common-dir in %s printed nothing", dir)
		}
		return out, err
	})
}

// RemoteURL returns the URL of remote as configured in dir (git remote get-url). Answers are
// cached when ctx carries a Cache, and dropped when a git command with that ctx fails in dir.
func RemoteURL(ctx context.Context, dir, remote string) (string, error) {
	return lookup(ctx, cacheKey{kind: "remote", dir: cacheDir(dir), remote: remote}, func() (string, error) {
		return Run(ctx, dir, "remote", "get-url", "--", remote)
	})
}
