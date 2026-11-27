package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/store"
	"github.com/msidorkevich/mill/internal/taskspec"
)

// hookLog configures both hooks to append their stdin, one document per line, to a file and
// returns the file's path.
func hookLog(t *testing.T, o *Options) string {
	t.Helper()
	log := filepath.Join(t.TempDir(), "hook.log")
	cmd := "cat >> '" + log + "'; echo >> '" + log + "'"
	o.Cfg.OnPublish, o.Cfg.OnUpdate = cmd, cmd
	return log
}

// hookDocs is every document the hooks received, in order.
func hookDocs(t *testing.T, log string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var docs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("hook stdin %q is not JSON: %v", line, err)
		}
		docs = append(docs, m)
	}
	return docs
}

// advance commits another change on the task's branch and records it as the gated head.
func advance(t *testing.T, o Options, br store.Branch, name string) store.Branch {
	t.Helper()
	ctx := context.Background()
	cur, err := o.Store.GetBranch(ctx, br.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cur.Worktree, name), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, cur.Worktree, "add", name)
	git(t, cur.Worktree, "commit", "-q", "-m", "more work")
	cur.HeadSHA = git(t, cur.Worktree, "rev-parse", "HEAD")
	cur.GateSHA = cur.HeadSHA
	if err := o.Store.SaveBranch(ctx, cur); err != nil {
		t.Fatal(err)
	}
	return cur
}

func publishedOntos(t *testing.T, s *store.Store, id string) []string {
	t.Helper()
	evs, err := s.TaskEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		if e.Kind != "task-published" {
			continue
		}
		var d map[string]string
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d["onto"])
	}
	return out
}

func hookRuns(t *testing.T, s *store.Store, id string) int {
	t.Helper()
	evs, err := s.TaskEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range evs {
		if e.Kind == "hook-run" {
			n++
		}
	}
	return n
}

func TestHookFirstPublishRunsOnPublishOnce(t *testing.T) {
	t.Parallel()
	o, _, _ := fixture(t)
	log := hookLog(t, &o)
	var stderr bytes.Buffer
	o.Stderr = &stderr

	res, err := Publish(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	docs := hookDocs(t, log)
	if len(docs) != 1 {
		t.Fatalf("hook ran %d times, want once: %v", len(docs), docs)
	}
	d := docs[0]
	if d["event"] != "publish" || d["task_id"] != "t1" || d["head_sha"] != res.SHA || d["base_branch"] != "main" {
		t.Errorf("document = %v", d)
	}
	if _, ok := d["previous_base"]; ok {
		t.Errorf("first publish has previous_base: %v", d)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q", stderr.String())
	}
	if got := publishedOntos(t, o.Store, "t1"); len(got) != 1 || got[0] != "main" {
		t.Errorf("task-published ontos = %q, want [main]", got)
	}
}

func TestHookRepublishRunsOnUpdateWithoutPreviousBaseWhenTargetUnchanged(t *testing.T) {
	t.Parallel()
	o, _, br := fixture(t)
	log := hookLog(t, &o)
	o.Stderr = &bytes.Buffer{}
	if _, err := Publish(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	advance(t, o, br, "y.txt")
	res, err := Publish(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Republish {
		t.Fatalf("result = %+v, want a re-publish", res)
	}
	docs := hookDocs(t, log)
	if len(docs) != 2 || docs[0]["event"] != "publish" || docs[1]["event"] != "update" {
		t.Fatalf("hook documents = %v, want publish then update", docs)
	}
	if docs[1]["head_sha"] != res.SHA {
		t.Errorf("update head_sha = %v, want %s", docs[1]["head_sha"], res.SHA)
	}
	if _, ok := docs[1]["previous_base"]; ok {
		t.Errorf("update with an unchanged target has previous_base: %v", docs[1])
	}
}

// stackedFixture is a repo with a bare remote and goal g1 whose verified task b is stacked on
// verified task a.
func stackedFixture(t *testing.T) (Options, store.Branch, store.Branch) {
	t.Helper()
	o, _, _ := fixture(t)
	ctx := context.Background()
	s, root := o.Store, o.Paths.Root
	base := git(t, root, "rev-parse", "HEAD")
	pl := plan.Plan{Goal: "stack", Tasks: []taskspec.Spec{
		{ID: "a", Title: "Task a", Kind: "implement", Spec: "write", Acceptance: []string{"ok"}, Owns: []string{}, Size: "S"},
		{ID: "b", Title: "Task b", Kind: "implement", Spec: "write", Acceptance: []string{"ok"}, Owns: []string{}, Size: "S", DependsOn: []string{"a"}},
	}}
	raw, err := json.Marshal(pl)
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.CreateGoal(ctx, "stack", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveGoal(ctx, g.ID, pl); err != nil {
		t.Fatal(err)
	}
	commit := func(dir, name string) string {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, dir, "add", name)
		git(t, dir, "commit", "-q", "-m", "work on "+name)
		return git(t, dir, "rev-parse", "HEAD")
	}
	aWT, bWT := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	git(t, root, "worktree", "add", "-q", "-b", "mill/"+g.ID+"/a", aWT, base)
	aHead := commit(aWT, "a.txt")
	git(t, root, "worktree", "add", "-q", "-b", "mill/"+g.ID+"/b", bWT, aHead)
	bHead := commit(bWT, "b.txt")
	a := store.Branch{TaskID: g.ID + "/a", Name: "mill/" + g.ID + "/a", Base: "main", BaseSHA: base, StartSHA: base, HeadSHA: aHead, GateSHA: aHead, Worktree: aWT}
	b := store.Branch{TaskID: g.ID + "/b", Name: "mill/" + g.ID + "/b", Base: a.Name, BaseSHA: aHead, StartSHA: aHead, HeadSHA: bHead, GateSHA: bHead, Worktree: bWT}
	for _, br := range []store.Branch{a, b} {
		if err := s.SaveBranch(ctx, br); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AppendEvent(ctx, "task-verdict", "", br.TaskID, map[string]any{"verdict": prompt.VerifierOutput{Verdict: "pass", Summary: "fine"}}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetTaskStatus(ctx, br.TaskID, store.TaskVerified, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	return o, a, b
}

func TestHookUpdateAfterParentLandsCarriesPreviousBase(t *testing.T) {
	t.Parallel()
	o, a, b := stackedFixture(t)
	ctx := context.Background()
	log := hookLog(t, &o)
	o.Stderr = &bytes.Buffer{}
	for _, id := range []string{a.TaskID, b.TaskID} {
		o.TaskID = id
		if _, err := Publish(ctx, o); err != nil {
			t.Fatal(err)
		}
	}

	// a lands on main; b is moved onto main and published again.
	root := o.Paths.Root
	git(t, root, "merge", "-q", "--ff-only", a.HeadSHA)
	git(t, root, "push", "-q", "origin", "main")
	if err := o.Store.SetTaskStatus(ctx, a.TaskID, store.TaskLanded, "", ""); err != nil {
		t.Fatal(err)
	}
	cur, err := o.Store.GetBranch(ctx, b.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	cur.Base, cur.BaseSHA = "main", a.HeadSHA
	if err := o.Store.SaveBranch(ctx, cur); err != nil {
		t.Fatal(err)
	}
	o.TaskID = b.TaskID
	res, err := Publish(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Republish || res.Onto != "main" {
		t.Fatalf("result = %+v, want a re-publish onto main", res)
	}

	docs := hookDocs(t, log)
	if len(docs) != 3 {
		t.Fatalf("hook ran %d times, want 3: %v", len(docs), docs)
	}
	first, last := docs[1], docs[2]
	if first["event"] != "publish" || first["task_id"] != b.TaskID || first["base_branch"] != "review/"+a.TaskID {
		t.Errorf("first publish of b = %v", first)
	}
	if last["event"] != "update" || last["task_id"] != b.TaskID || last["base_branch"] != "main" || last["previous_base"] != "review/"+a.TaskID {
		t.Errorf("update of b = %v, want base main and previous_base review/%s", last, a.TaskID)
	}
	if got := publishedOntos(t, o.Store, b.TaskID); len(got) != 2 || got[0] != "review/"+a.TaskID || got[1] != "main" {
		t.Errorf("task-published ontos of b = %q", got)
	}
}

func TestHookFailureLeavesTaskPublished(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, cmd, want string
		timeout         time.Duration
	}{
		{name: "exit", cmd: "echo boom; exit 3", want: "hook on_publish for t1 failed (exit 3): boom; re-run it with `mill hook run t1`\n", timeout: time.Minute},
		{name: "timeout", cmd: "sleep 30", want: "hook on_publish for t1 failed (timed out after 100ms)", timeout: 100 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o, bare, _ := fixture(t)
			o.Cfg.OnPublish, o.Cfg.HookTimeout.Duration = tc.cmd, tc.timeout
			var stderr bytes.Buffer
			o.Stderr = &stderr
			res, err := Publish(context.Background(), o)
			if err != nil {
				t.Fatalf("Publish with a failing hook = %v, want nil", err)
			}
			if res.SHA == "" || res.Onto != "main" || res.Republish {
				t.Errorf("result = %+v", res)
			}
			if got := git(t, bare, "rev-parse", "refs/heads/review/t1"); got != res.SHA {
				t.Errorf("remote review/t1 = %s, want %s", got, res.SHA)
			}
			tk, _ := o.Store.GetTask(context.Background(), "t1")
			br, _ := o.Store.GetBranch(context.Background(), "t1")
			if tk.Status != store.TaskPublished || br.PublishedSHA != res.SHA {
				t.Errorf("task %s, published %s; want published at %s", tk.Status, br.PublishedSHA, res.SHA)
			}
			if !strings.HasPrefix(stderr.String(), tc.want) || !strings.HasSuffix(stderr.String(), "re-run it with `mill hook run t1`\n") {
				t.Errorf("stderr = %q, want it to start with %q", stderr.String(), tc.want)
			}
		})
	}
}

func TestHookNotConfiguredRunsNothing(t *testing.T) {
	t.Parallel()
	o, _, br := fixture(t)
	var stderr bytes.Buffer
	o.Stderr = &stderr
	if _, err := Publish(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	advance(t, o, br, "y.txt")
	if _, err := Publish(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if n := hookRuns(t, o.Store, "t1"); n != 0 {
		t.Errorf("%d hook-run events, want none", n)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", stderr.String())
	}
}
