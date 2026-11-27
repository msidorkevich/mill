// Package hook builds what mill hands to the operator's hook scripts (HK1, HK2). The core
// never knows which forge a hook talks to: the document carries only git and mill facts.
package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/gate"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/pipeline"
	"github.com/msidorkevich/mill/internal/plan"
	"github.com/msidorkevich/mill/internal/prompt"
	"github.com/msidorkevich/mill/internal/skiptest"
	"github.com/msidorkevich/mill/internal/store"
)

// Schema is the version of the document a hook receives on stdin.
const Schema = "mill/hook@1"

// The events a hook runs for.
const (
	EventPublish = "publish" // after a task's first publish
	EventUpdate  = "update"  // after a later push of a published task, or a change of its base
)

// Document is the JSON a hook reads on stdin.
type Document struct {
	Schema      string `json:"schema"`
	Event       string `json:"event"`
	GoalID      string `json:"goal_id"`
	TaskID      string `json:"task_id"`
	Title       string `json:"title"`
	Description string `json:"description"` // the published commit message body
	// ReviewBranch is the branch the task is published to; BaseBranch is the branch its PR
	// targets: the parent's review branch for a stacked task, otherwise the base branch.
	ReviewBranch string `json:"review_branch"`
	BaseBranch   string `json:"base_branch"`
	// PreviousBase is the base branch before an update changed it; empty when it did not.
	PreviousBase string `json:"previous_base,omitempty"`
	HeadSHA      string `json:"head_sha"` // the published commit
	Remote       string `json:"remote"`
	RemoteURL    string `json:"remote_url"`
	// ParentTaskID and ParentReviewBranch are set for a stacked task: its parent is
	// published and has not landed.
	ParentTaskID       string `json:"parent_task_id,omitempty"`
	ParentReviewBranch string `json:"parent_review_branch,omitempty"`
	// StackPosition is 1 for the bottom of a stack, 2 for the task stacked on it, and so on.
	StackPosition int             `json:"stack_position"`
	Verifier      Verifier        `json:"verifier"`
	Gate          Gate            `json:"gate"`
	CostUSD       float64         `json:"cost_usd"`
	SkippedTests  []skiptest.Skip `json:"skipped_tests"` // the test skips the task added
}

// Verifier is the task's latest verdict.
type Verifier struct {
	Verdict string `json:"verdict"`
	Summary string `json:"summary"`
}

// Gate is the task's latest gate result and the commit it ran at.
type Gate struct {
	Passed bool   `json:"passed"`
	SHA    string `json:"sha"`
}

// Build assembles the document for a published task. root is the repository, event one of
// EventPublish and EventUpdate, previousBase the base branch before an update ("" when it did
// not change).
func Build(ctx context.Context, s *store.Store, cfg config.Config, root string, taskID, event, previousBase string) (Document, error) {
	t, err := s.GetTask(ctx, taskID)
	if err != nil {
		return Document{}, fmt.Errorf("build the hook document of %s: %w", taskID, err)
	}
	if t.Status != store.TaskPublished {
		return Document{}, fmt.Errorf("task %s is %s, not published, so there is nothing for a hook to describe; publish it first with `mill review approve %s`",
			taskID, t.Status, taskID)
	}
	br, err := s.GetBranch(ctx, taskID)
	if err != nil {
		return Document{}, fmt.Errorf("task %s is published but its branch cannot be loaded: %w", taskID, err)
	}
	if br.PublishedRef == "" || br.PublishedSHA == "" {
		return Document{}, fmt.Errorf("task %s is published but has no recorded published commit; publish it again with `mill review approve %s`", taskID, taskID)
	}
	d := Document{
		Schema: Schema, Event: event, GoalID: t.GoalID, TaskID: t.ID, Title: t.Title,
		ReviewBranch: br.PublishedRef, HeadSHA: br.PublishedSHA, PreviousBase: previousBase,
		Remote: cfg.Remote, BaseBranch: cfg.BaseBranch, StackPosition: 1,
		SkippedTests: []skiptest.Skip{},
	}

	d.Description, err = gitx.Run(ctx, root, "log", "-1", "--format=%b", br.PublishedSHA)
	if err != nil {
		return Document{}, fmt.Errorf("read the published commit %s of %s; fetch %s and try again: %w", br.PublishedSHA, taskID, cfg.Remote, err)
	}
	d.RemoteURL, err = gitx.RemoteURL(ctx, root, cfg.Remote)
	if err != nil {
		return Document{}, fmt.Errorf("remote %q is not configured in git; add it with `git remote add %s <url>` or set `remote` in the config: %w",
			cfg.Remote, cfg.Remote, err)
	}

	// The stack: the direct parent sets the base, every published ancestor below adds one.
	seen := map[string]bool{taskID: true}
	for cur, first := taskID, true; ; first = false {
		p, err := directParent(ctx, s, cur)
		if err != nil {
			return Document{}, err
		}
		if p == "" || seen[p] {
			break
		}
		seen[p] = true
		pt, err := s.GetTask(ctx, p)
		if err != nil {
			return Document{}, fmt.Errorf("load %s, an ancestor of %s: %w", p, taskID, err)
		}
		if pt.Status != store.TaskPublished {
			break
		}
		if first {
			pb, err := s.GetBranch(ctx, p)
			if err != nil {
				return Document{}, fmt.Errorf("parent %s of %s is published but its branch cannot be loaded: %w", p, taskID, err)
			}
			ref := pb.PublishedRef
			if ref == "" {
				ref = gitx.ReviewPrefix + p
			}
			d.ParentTaskID, d.ParentReviewBranch, d.BaseBranch = p, ref, ref
		}
		d.StackPosition++
		cur = p
	}

	evs, err := s.TaskEvents(ctx, taskID)
	if err != nil {
		return Document{}, fmt.Errorf("read the events of %s: %w", taskID, err)
	}
	if err := latest(evs, &d, br); err != nil {
		return Document{}, fmt.Errorf("task %s: %w", taskID, err)
	}
	usage, err := s.TaskUsage(ctx, taskID)
	if err != nil {
		return Document{}, fmt.Errorf("read the cost of %s: %w", taskID, err)
	}
	d.CostUSD = usage.CostUSD
	return d, nil
}

// directParent is the dependency a task is stacked on: its only dependency, or for a fan-in
// task the plan's deepest parent (as publish decides). "" when the task has none.
func directParent(ctx context.Context, s *store.Store, id string) (string, error) {
	deps, err := s.TaskDeps(ctx, id)
	if err != nil {
		return "", fmt.Errorf("read the dependencies of %s: %w", id, err)
	}
	switch len(deps) {
	case 0:
		return "", nil
	case 1:
		return deps[0], nil
	}
	goalID, local := store.SplitTaskID(id)
	g, err := s.GetGoal(ctx, goalID)
	if err != nil {
		return "", fmt.Errorf("load goal %s of %s: %w", goalID, id, err)
	}
	var pl plan.Plan
	if err := json.Unmarshal(g.Plan, &pl); err != nil {
		return "", fmt.Errorf("goal %s has an unreadable plan: %w", goalID, err)
	}
	deepest := pl.DeepestParent(local)
	if deepest == "" {
		return "", fmt.Errorf("goal %s plan has no task %s, so the parent of %s cannot be told", goalID, local, id)
	}
	return store.TaskID(goalID, deepest), nil
}

// latest fills the verifier, gate and skipped tests from the task's newest events of each
// kind. Without a gate event the branch's recorded green gate is used.
func latest(evs []store.Event, d *Document, br store.Branch) error {
	var verdict, gated, skips bool
	for i := len(evs) - 1; i >= 0 && !(verdict && gated && skips); i-- {
		e := evs[i]
		switch {
		case e.Kind == "task-verdict" && !verdict:
			var v struct {
				Verdict prompt.VerifierOutput `json:"verdict"`
			}
			if err := json.Unmarshal(e.Data, &v); err != nil {
				return fmt.Errorf("corrupt task-verdict event %d: %w", e.Seq, err)
			}
			d.Verifier = Verifier{Verdict: v.Verdict.Verdict, Summary: v.Verdict.Summary}
			verdict = true
		case e.Kind == "task-gate" && !gated:
			var g struct {
				Head   string      `json:"head"`
				Result gate.Result `json:"result"`
			}
			if err := json.Unmarshal(e.Data, &g); err != nil {
				return fmt.Errorf("corrupt task-gate event %d: %w", e.Seq, err)
			}
			d.Gate = Gate{Passed: g.Result.Passed, SHA: g.Head}
			gated = true
		case (e.Kind == pipeline.SkippedTestsEvent || e.Kind == pipeline.OldSkippedTestsEvent) && !skips:
			var sk struct {
				Skips []skiptest.Skip `json:"skips"`
			}
			if err := json.Unmarshal(e.Data, &sk); err != nil {
				return fmt.Errorf("corrupt %s event %d: %w", e.Kind, e.Seq, err)
			}
			if sk.Skips != nil {
				d.SkippedTests = sk.Skips
			}
			skips = true
		}
	}
	if !verdict {
		return errors.New("no recorded verdict; a published task always has one, so check the mill database")
	}
	if !gated && br.GateSHA != "" {
		d.Gate = Gate{Passed: br.GateSHA == br.HeadSHA, SHA: br.GateSHA}
	}
	return nil
}
