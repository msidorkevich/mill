package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/msidorkevich/mill/internal/store"
)

// keepQueueDoc is the part of a mill keep --json document the queue tests read.
type keepQueueDoc struct {
	Schema string `json:"schema"`
	Keeper struct {
		Sync struct {
			Landed []struct {
				TaskID string `json:"task_id"`
				Commit string `json:"commit"`
			} `json:"landed"`
			Restacked []struct {
				TaskID  string `json:"task_id"`
				OntoSHA string `json:"onto_sha"`
				NewHead string `json:"new_head"`
			} `json:"restacked"`
		} `json:"sync"`
		Blocked []struct {
			TaskID string `json:"task_id"`
			Reason string `json:"reason"`
		} `json:"blocked"`
		Queue struct {
			Published []struct {
				TaskID string `json:"task_id"`
				Ref    string `json:"ref"`
				SHA    string `json:"sha"`
			} `json:"published"`
			Waiting []struct {
				TaskID string `json:"task_id"`
				Reason string `json:"reason"`
			} `json:"waiting"`
		} `json:"queue"`
	} `json:"keeper"`
}

// keepJSON runs one mill keep --json pass and decodes it; raw is stdout.
func keepJSON(t *testing.T, root string) (doc keepQueueDoc, raw string) {
	t.Helper()
	r := run(t, root, nil, "keep", "--json")
	if r.code != ExitOK {
		t.Fatalf("keep --json: %+v", r)
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("keep --json is not JSON: %v\n%s", err, r.stdout)
	}
	if doc.Schema != "mill/keep@1" {
		t.Errorf("schema = %q", doc.Schema)
	}
	return doc, r.stdout
}

// queueChild runs the chain goal g2 (b depends on a) to verified and approves the child,
// which queues because its parent is not published yet.
func queueChild(t *testing.T) (root, bare string) {
	t.Helper()
	root, bare = chainPublished(t, false)
	r := run(t, root, nil, "review", "approve", "g2/b")
	if r.code != ExitOK || !strings.Contains(r.stdout, "approved g2/b: queued") {
		t.Fatalf("approve child: %+v", r)
	}
	return root, bare
}

func TestKeepPublishesQueuedChildAfterParentLands(t *testing.T) {
	root, bare := queueChild(t)
	ctx := context.Background()
	s := openStore(t, root)

	// The child is queued; approving the parent publishes it. (`review approve --goal g2`
	// would publish the child on the published parent in the same run, so the parent is
	// approved on its own to keep the child queued until the parent lands.)
	r := run(t, root, nil, "review", "approve", "g2/a")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Published g2/a ") {
		t.Fatalf("approve parent: %+v", r)
	}
	if _, ok := remoteRefs(t, bare)["refs/heads/review/g2/b"]; ok {
		t.Fatal("child published before its parent landed")
	}

	merge := forgeMerge(t, bare, func(f string) {
		gitC(t, f, "merge", "-q", "--no-ff", "-m", "Merge review/g2/a", "origin/review/g2/a")
	})

	doc, raw := keepJSON(t, root)
	k := doc.Keeper
	if len(k.Sync.Landed) != 1 || k.Sync.Landed[0].TaskID != "g2/a" || k.Sync.Landed[0].Commit != merge {
		t.Errorf("landed = %+v\n%s", k.Sync.Landed, raw)
	}
	if len(k.Sync.Restacked) != 1 || k.Sync.Restacked[0].TaskID != "g2/b" || k.Sync.Restacked[0].OntoSHA != merge {
		t.Errorf("restacked = %+v\n%s", k.Sync.Restacked, raw)
	}
	if len(k.Queue.Published) != 1 || k.Queue.Published[0].TaskID != "g2/b" || k.Queue.Published[0].Ref != "review/g2/b" {
		t.Fatalf("queue.published = %+v\n%s", k.Queue.Published, raw)
	}
	if len(k.Queue.Waiting) != 0 || len(k.Blocked) != 0 {
		t.Errorf("waiting = %+v, blocked = %+v", k.Queue.Waiting, k.Blocked)
	}

	if tk, _ := s.GetTask(ctx, "g2/a"); tk.Status != store.TaskLanded {
		t.Errorf("g2/a status = %s, want landed", tk.Status)
	}
	child, err := s.GetBranch(ctx, "g2/b")
	if err != nil {
		t.Fatal(err)
	}
	if child.Base != "main" || child.BaseSHA != merge || child.HeadSHA != k.Sync.Restacked[0].NewHead {
		t.Errorf("child branch = %+v, want based on main at %s", child, merge)
	}
	pub := remoteRefs(t, bare)["refs/heads/review/g2/b"]
	if pub == "" || pub != k.Queue.Published[0].SHA || pub != child.PublishedSHA {
		t.Fatalf("review/g2/b = %q, queue sha %s, recorded %s", pub, k.Queue.Published[0].SHA, child.PublishedSHA)
	}
	// The published commit is the restacked head squashed onto the merge.
	if got := gitC(t, bare, "rev-list", "--parents", "-n1", pub); got != pub+" "+merge {
		t.Errorf("published parents = %q, want %s", got, merge)
	}
	if got, want := gitC(t, root, "rev-parse", pub+"^{tree}"), gitC(t, root, "rev-parse", child.HeadSHA+"^{tree}"); got != want {
		t.Errorf("published tree %s, restacked head tree %s", got, want)
	}
	tk, err := s.GetTask(ctx, "g2/b")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Status != store.TaskPublished {
		t.Errorf("g2/b status = %s, want published", tk.Status)
	}

	// Nothing is left in the queue.
	doc, raw = keepJSON(t, root)
	if len(doc.Keeper.Queue.Published) != 0 || len(doc.Keeper.Queue.Waiting) != 0 {
		t.Errorf("second pass queue = %+v\n%s", doc.Keeper.Queue, raw)
	}
}

func TestKeepListsQueuedTaskWaitingForUnpublishedParent(t *testing.T) {
	root, bare := queueChild(t)

	r := run(t, root, nil, "keep")
	if r.code != ExitOK {
		t.Fatalf("keep: %+v", r)
	}
	i := strings.Index(r.stdout, "Waiting:\n  g2/b  ")
	if i < 0 || !strings.Contains(r.stdout[i:], "g2/a") || strings.Contains(r.stdout, "Published from queue") {
		t.Errorf("keep output lacks the waiting child:\n%s", r.stdout)
	}

	doc, raw := keepJSON(t, root)
	q := doc.Keeper.Queue
	if q.Published == nil || len(q.Published) != 0 {
		t.Errorf("queue.published = %v, want []\n%s", q.Published, raw)
	}
	if len(q.Waiting) != 1 || q.Waiting[0].TaskID != "g2/b" || !strings.Contains(q.Waiting[0].Reason, "g2/a") {
		t.Errorf("queue.waiting = %+v\n%s", q.Waiting, raw)
	}
	if _, ok := remoteRefs(t, bare)["refs/heads/review/g2/b"]; ok {
		t.Error("waiting child was pushed")
	}
	tk, err := openStore(t, root).GetTask(context.Background(), "g2/b")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Status != store.TaskVerified || !tk.Approved {
		t.Errorf("g2/b = %s approved=%v, want verified and approved", tk.Status, tk.Approved)
	}
}

func TestKeepBlocksQueuedTaskWhosePublishIsRefused(t *testing.T) {
	root, bare := queueChild(t)
	ctx := context.Background()
	if r := run(t, root, nil, "review", "approve", "g2/a"); r.code != ExitOK {
		t.Fatalf("approve parent: %+v", r)
	}
	// The child's gate must be re-run at publish, and now it fails.
	s := openStore(t, root)
	br, err := s.GetBranch(ctx, "g2/b")
	if err != nil {
		t.Fatal(err)
	}
	br.GateSHA = ""
	if err := s.SaveBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	writeCheckCmd(t, root, "exit 7")

	r := run(t, root, nil, "keep")
	if r.code != ExitOK {
		t.Fatalf("keep: %+v", r)
	}
	i := strings.Index(r.stdout, "Blocked:\n  g2/b  ")
	if i < 0 || !strings.Contains(r.stdout[i:], "publishing the approved task failed") || !strings.Contains(r.stdout[i:], "the gate must be green") {
		t.Errorf("keep output lacks the blocked child:\n%s", r.stdout)
	}
	tk, err := s.GetTask(ctx, "g2/b")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Status != store.TaskBlocked || !strings.Contains(tk.Reason, "the gate must be green") {
		t.Errorf("g2/b = %s (%s), want blocked by the gate refusal", tk.Status, tk.Reason)
	}
	if _, ok := remoteRefs(t, bare)["refs/heads/review/g2/b"]; ok {
		t.Error("refused child was pushed")
	}
}

func TestReviewChangesRemovesTaskFromQueue(t *testing.T) {
	root, bare := queueChild(t)
	ctx := context.Background()
	if r := run(t, root, nil, "review", "changes", "g2/b", "-m", "rename the file"); r.code != ExitOK {
		t.Fatalf("review changes: %+v", r)
	}
	tk, err := openStore(t, root).GetTask(ctx, "g2/b")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Approved {
		t.Error("g2/b still approved after review changes")
	}
	if ev := lastEvent(t, openStore(t, root), "g2/b", "task-approval-cleared"); !strings.Contains(ev["reason"].(string), "changes requested") {
		t.Errorf("task-approval-cleared = %v", ev)
	}

	// Even with the parent published, nothing publishes the child from the queue.
	if r := run(t, root, nil, "review", "approve", "g2/a"); r.code != ExitOK {
		t.Fatalf("approve parent: %+v", r)
	}
	doc, raw := keepJSON(t, root)
	if q := doc.Keeper.Queue; len(q.Published) != 0 || len(q.Waiting) != 0 {
		t.Errorf("queue = %+v\n%s", q, raw)
	}
	if _, ok := remoteRefs(t, bare)["refs/heads/review/g2/b"]; ok {
		t.Error("child published after changes were requested")
	}
}
