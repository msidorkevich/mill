// Package publish turns a verified task into one squashed commit pushed to review/<task> on the
// configured remote (RV4–RV6). It is shared by `mill review approve` and the keeper's sync.
//
// The caller holds the run lock. Every refusal names the RV5 rule that failed.
package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mill/internal/config"
	"mill/internal/gate"
	"mill/internal/gitx"
	"mill/internal/host"
	"mill/internal/plan"
	"mill/internal/project"
	"mill/internal/prompt"
	"mill/internal/store"
)

// Options configure one publish.
type Options struct {
	Store  *store.Store
	Cfg    config.Config
	Paths  project.Paths
	Host   host.Host // runs the gate when it must be re-run; nil means host.NewExec()
	TaskID string
}

// Result is what was published.
type Result struct {
	TaskID    string       `json:"task_id"`
	Remote    string       `json:"remote"`
	Ref       string       `json:"ref"`        // review/<id>
	SHA       string       `json:"sha"`        // the pushed commit
	Parent    string       `json:"parent"`     // its parent commit
	Onto      string       `json:"onto"`       // the branch a PR should target: review/<parent> or the base branch
	Republish bool         `json:"republish"`  // the task was already published
	GateRerun *gate.Result `json:"gate_rerun"` // set when the gate was re-run at the head
}

// RefusedError is a publish that a RV5 rule does not allow. Nothing was pushed.
type RefusedError struct {
	Rule string // short name of the rule
	Msg  string
}

func (e *RefusedError) Error() string { return fmt.Sprintf("refused (%s): %s", e.Rule, e.Msg) }

const (
	ruleStatus   = "the task must be verified"
	ruleGate     = "the gate must be green at the current head"
	ruleConflict = "no conflict with the base"
	ruleParent   = "dependencies must be published or landed"
	ruleRemote   = "a configured remote"
)

func refuse(rule, format string, a ...any) error {
	return &RefusedError{Rule: rule, Msg: fmt.Sprintf(format, a...)}
}

// ReviewRef is the remote branch a task is published to.
func ReviewRef(taskID string) string { return gitx.ReviewPrefix + taskID }

// stack is where a task sits: the commit its squash goes on and what that commit is.
type stack struct {
	parentSHA  string // parent of the published commit
	onto       string // review/<parent> or the base branch
	conflictTo string // the ref the task must merge cleanly into
	desc       string // "on review/<parent>" or "on <base> @<short>"
}

// Publish checks the RV5 rules for the task and, if they all hold, pushes one squashed commit
// to review/<id> on the configured remote and records the task as published.
func Publish(ctx context.Context, o Options) (Result, error) {
	s, cfg, root, id := o.Store, o.Cfg, o.Paths.Root, o.TaskID
	res := Result{TaskID: id, Remote: cfg.Remote, Ref: ReviewRef(id)}

	t, err := s.GetTask(ctx, id)
	if err != nil {
		return res, err
	}
	switch t.Status {
	case store.TaskVerified:
	case store.TaskPublished:
		res.Republish = true
	default:
		return res, refuse(ruleStatus, "task %s is %s: only a verified task (or a published one with new changes) can be published", id, t.Status)
	}
	br, err := s.GetBranch(ctx, id)
	if err != nil {
		return res, fmt.Errorf("task %s is %s but its branch cannot be loaded: %w", id, t.Status, err)
	}
	if br.HeadSHA == "" {
		return res, fmt.Errorf("task %s has no recorded head commit; run `mill task show %s`", id, id)
	}

	st, err := stackOf(ctx, s, cfg, t, br)
	if err != nil {
		return res, err
	}

	// The remote must exist in git before anything is fetched or pushed.
	if cfg.Remote == "" {
		return res, refuse(ruleRemote, "no remote is set; set `remote` in %s", o.Paths.Config)
	}
	if _, err := gitx.Run(ctx, root, "remote", "get-url", "--", cfg.Remote); err != nil {
		return res, refuse(ruleRemote, "remote %q is not configured in git; add it with `git remote add %s <url>` or set `remote` in %s to an existing remote",
			cfg.Remote, cfg.Remote, o.Paths.Config)
	}

	// What mill recorded must be what git has, or the gate and the squash would describe
	// different code.
	tip, err := gitx.RevParse(ctx, root, br.Name)
	if err != nil {
		return res, fmt.Errorf("branch %s of task %s is missing from git: %w", br.Name, id, err)
	}
	if tip != br.HeadSHA {
		return res, fmt.Errorf("branch %s is at %s but mill recorded head %s; send the task back with `mill review changes %s` so mill records its commits",
			br.Name, short(tip), short(br.HeadSHA), id)
	}

	// No conflict with the base, checked against the remote's current state.
	if err := gitx.Fetch(ctx, root, cfg.Remote); err != nil {
		return res, fmt.Errorf("fetch %s: %w", cfg.Remote, err)
	}
	conflictTo := st.conflictTo
	if conflictTo == "" {
		conflictTo = cfg.Remote + "/" + cfg.BaseBranch
		if _, err := gitx.RevParse(ctx, root, "refs/remotes/"+conflictTo); err != nil {
			return res, fmt.Errorf("remote %s has no branch %s, so the conflict check cannot run; push %s to %s first",
				cfg.Remote, cfg.BaseBranch, cfg.BaseBranch, cfg.Remote)
		}
	}
	files, err := gitx.MergeConflicts(ctx, root, conflictTo, br.HeadSHA)
	if err != nil {
		return res, fmt.Errorf("conflict check of %s against %s: %w", br.Name, conflictTo, err)
	}
	if len(files) > 0 {
		return res, refuse(ruleConflict, "%s conflicts with %s in %s; run `mill task rebase %s`",
			br.Name, conflictTo, strings.Join(files, ", "), id)
	}

	// Gate green at the current head.
	gateCmds := nonEmpty(append([]string{cfg.CheckCmd}, t.Checks...))
	if br.GateSHA != br.HeadSHA {
		g, err := rerunGate(ctx, o, t, br, gateCmds, "publish")
		if err != nil {
			return res, err
		}
		res.GateRerun = &g
		if !g.Passed {
			return res, refuse(ruleGate, "the gate was re-run at %s and failed:\n%s", short(br.HeadSHA), failedCmds(g))
		}
		br.GateSHA = br.HeadSHA
		if err := s.SaveBranch(ctx, br); err != nil {
			return res, fmt.Errorf("record the gate result at %s: %w", short(br.HeadSHA), err)
		}
	}

	tree, err := gitx.TreeOf(ctx, root, br.HeadSHA)
	if err != nil {
		return res, err
	}
	if res.Republish && br.PublishedSHA != "" {
		pubTree, err := gitx.TreeOf(ctx, root, br.PublishedSHA)
		if err == nil {
			pubParent, perr := gitx.RevParse(ctx, root, br.PublishedSHA+"^")
			if perr == nil && pubTree == tree && pubParent == st.parentSHA {
				return res, refuse(ruleStatus, "task %s is already published at %s with no new changes; nothing to publish", id, short(br.PublishedSHA))
			}
		}
	}

	msg, err := message(ctx, s, cfg, t, br, st, gateCmds)
	if err != nil {
		return res, err
	}
	sha, err := gitx.CommitTree(ctx, root, tree, st.parentSHA, msg)
	if err != nil {
		return res, fmt.Errorf("create the squashed commit: %w", err)
	}
	if err := gitx.PushReview(ctx, root, cfg.Remote, sha, res.Ref, br.PublishedSHA); err != nil {
		lease := "it must not exist yet"
		if br.PublishedSHA != "" {
			lease = "it must still be at " + short(br.PublishedSHA)
		}
		return res, fmt.Errorf("push %s to %s (%s): %w; if someone else moved the branch, fetch and inspect it before publishing again",
			res.Ref, cfg.Remote, lease, err)
	}
	res.SHA, res.Parent, res.Onto = sha, st.parentSHA, st.onto

	br.PublishedRef, br.PublishedSHA = res.Ref, sha
	if err := s.SaveBranch(ctx, br); err != nil {
		return res, fmt.Errorf("pushed %s to %s/%s but recording it failed: %w", short(sha), cfg.Remote, res.Ref, err)
	}
	if err := s.SetTaskStatus(ctx, id, store.TaskPublished, "", ""); err != nil {
		return res, fmt.Errorf("pushed %s to %s/%s but recording it failed: %w", short(sha), cfg.Remote, res.Ref, err)
	}
	if _, err := s.AppendEvent(ctx, "task-published", t.GoalID, id,
		map[string]string{"sha": sha, "parent": st.parentSHA, "remote": cfg.Remote, "ref": res.Ref}); err != nil {
		return res, fmt.Errorf("pushed %s to %s/%s but recording the event failed: %w", short(sha), cfg.Remote, res.Ref, err)
	}
	return res, nil
}

// stackOf checks the dependency rule and decides the published commit's parent.
func stackOf(ctx context.Context, s *store.Store, cfg config.Config, t store.Task, br store.Branch) (stack, error) {
	deps, err := s.TaskDeps(ctx, t.ID)
	if err != nil {
		return stack{}, err
	}
	if len(deps) == 0 {
		if br.BaseSHA == "" {
			return stack{}, fmt.Errorf("task %s has no recorded base commit", t.ID)
		}
		return stack{parentSHA: br.BaseSHA, onto: cfg.BaseBranch, desc: fmt.Sprintf("on %s @%s", cfg.BaseBranch, short(br.BaseSHA))}, nil
	}
	parent := deps[0]
	if len(deps) > 1 {
		goalID, local := store.SplitTaskID(t.ID)
		g, err := s.GetGoal(ctx, goalID)
		if err != nil {
			return stack{}, err
		}
		var pl plan.Plan
		if err := json.Unmarshal(g.Plan, &pl); err != nil {
			return stack{}, fmt.Errorf("goal %s has an unreadable plan: %w", goalID, err)
		}
		deepest := pl.DeepestParent(local)
		if deepest == "" {
			return stack{}, fmt.Errorf("goal %s plan has no task %s", goalID, local)
		}
		parent = store.TaskID(goalID, deepest)
		var notLanded []string
		found := false
		for _, d := range deps {
			if d == parent {
				found = true
				continue
			}
			dt, err := s.GetTask(ctx, d)
			if err != nil {
				return stack{}, err
			}
			if dt.Status != store.TaskLanded {
				notLanded = append(notLanded, fmt.Sprintf("%s is %s", d, dt.Status))
			}
		}
		if !found {
			return stack{}, fmt.Errorf("plan parent %s of %s is not among its stored dependencies", parent, t.ID)
		}
		if len(notLanded) > 0 {
			return stack{}, refuse(ruleParent, "fan-in task %s is stacked on %s; every other dependency must have landed first, but %s",
				t.ID, parent, strings.Join(notLanded, ", "))
		}
	}
	pt, err := s.GetTask(ctx, parent)
	if err != nil {
		return stack{}, err
	}
	switch pt.Status {
	case store.TaskPublished:
		pb, err := s.GetBranch(ctx, parent)
		if err != nil {
			return stack{}, fmt.Errorf("parent %s is published but its branch cannot be loaded: %w", parent, err)
		}
		if pb.PublishedSHA == "" || pb.PublishedRef == "" {
			return stack{}, fmt.Errorf("parent %s is published but has no recorded published commit; publish it again with `mill review approve %s`", parent, parent)
		}
		return stack{parentSHA: pb.PublishedSHA, onto: pb.PublishedRef, conflictTo: pb.Name, desc: "on " + pb.PublishedRef}, nil
	case store.TaskLanded:
		if br.BaseSHA == "" {
			return stack{}, fmt.Errorf("task %s has no recorded base commit", t.ID)
		}
		return stack{parentSHA: br.BaseSHA, onto: cfg.BaseBranch, desc: fmt.Sprintf("on %s @%s", cfg.BaseBranch, short(br.BaseSHA))}, nil
	}
	return stack{}, refuse(ruleParent, "task %s is stacked on %s, which is %s; publish it first with `mill review approve %s`",
		t.ID, parent, pt.Status, parent)
}

// GateAtHead re-runs the task's gate (check_cmd and its checks) at the branch head in its
// worktree and records a task-gate event. phase names the log directory (runs/<id>/<phase>) and
// is recorded in the event. It does not change the branch row: the caller records gate_sha.
func GateAtHead(ctx context.Context, o Options, t store.Task, br store.Branch, phase string) (gate.Result, error) {
	return rerunGate(ctx, o, t, br, nonEmpty(append([]string{o.Cfg.CheckCmd}, t.Checks...)), phase)
}

// FailedCommands lists the failing commands of a gate result, one per line.
func FailedCommands(g gate.Result) string { return failedCmds(g) }

// rerunGate runs check_cmd and the task's checks in the worktree, as the pipeline does.
func rerunGate(ctx context.Context, o Options, t store.Task, br store.Branch, cmds []string, phase string) (gate.Result, error) {
	if br.Worktree == "" {
		return gate.Result{}, fmt.Errorf("task %s has no worktree to re-run the gate in", t.ID)
	}
	if !gitx.IsWorktreeOn(ctx, br.Worktree, br.Name) {
		return gate.Result{}, fmt.Errorf("%s is not a worktree on %s, so the gate cannot be re-run at the head", br.Worktree, br.Name)
	}
	if dirty, err := gitx.HasChanges(ctx, br.Worktree); err != nil {
		return gate.Result{}, err
	} else if dirty {
		return gate.Result{}, fmt.Errorf("worktree %s has uncommitted changes; commit or discard them before publishing", br.Worktree)
	}
	h := o.Host
	if h == nil {
		h = host.NewExec()
	}
	g := gate.Run(ctx, gate.Options{
		Host: h, Dir: br.Worktree, Cmds: cmds, LogDir: filepath.Join(o.Paths.Runs, t.ID, phase),
		Env:     host.AllowEnv(os.Environ(), append(append([]string{}, host.DefaultEnvAllow...), o.Cfg.EnvAllow...)),
		Timeout: o.Cfg.GateTimeout.Duration,
	})
	if ctx.Err() != nil {
		return g, fmt.Errorf("gate re-run interrupted: %w", ctx.Err())
	}
	n, err := o.Store.MaxAttemptN(ctx, t.ID)
	if err != nil {
		return g, err
	}
	if _, err := o.Store.AppendEvent(ctx, "task-gate", "", t.ID, map[string]any{"attempt": n, "head": br.HeadSHA, "result": g, phase: true}); err != nil {
		return g, err
	}
	return g, nil
}

func failedCmds(g gate.Result) string {
	if len(g.Commands) == 0 {
		return "  " + g.Reason
	}
	var b strings.Builder
	for _, c := range g.Commands {
		if c.Passed() {
			continue
		}
		what := fmt.Sprintf("exit %d", c.Exit)
		if c.Error != "" {
			what = c.Error
		}
		fmt.Fprintf(&b, "  %s (%s; log %s)\n", c.Cmd, what, c.LogPath)
	}
	return strings.TrimRight(b.String(), "\n")
}

// message is the published commit's description (RV6).
func message(ctx context.Context, s *store.Store, cfg config.Config, t store.Task, br store.Branch, st stack, gateCmds []string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", oneLine(t.Title))
	fmt.Fprintf(&b, "Task: %s\n", t.ID)
	if t.GoalID != "" {
		g, err := s.GetGoal(ctx, t.GoalID)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "Goal: %s (%s)\n", oneLine(g.Title), g.ID)
	}
	fmt.Fprintf(&b, "Stacked %s\n", st.desc)
	if len(t.Acceptance) > 0 {
		b.WriteString("\nAcceptance:\n")
		for _, a := range t.Acceptance {
			fmt.Fprintf(&b, "- [ ] %s\n", oneLine(a))
		}
	}
	fmt.Fprintf(&b, "\nGate: gate passed at %s\n", short(br.HeadSHA))
	for _, c := range gateCmds {
		fmt.Fprintf(&b, "  %s\n", oneLine(c))
	}
	summary, err := verifierSummary(ctx, s, t.ID)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "\nVerifier: %s\n", summary)
	usage, err := s.TaskUsage(ctx, t.ID)
	if err != nil {
		return "", err
	}
	attempts, err := s.MaxAttemptN(ctx, t.ID)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "Cost: $%.2f over %d attempt(s)\n", usage.CostUSD, attempts)
	fmt.Fprintf(&b, "\nMill-Task: %s\n", t.ID)
	if t.GoalID != "" {
		fmt.Fprintf(&b, "Mill-Goal: %s\n", t.GoalID)
	}
	fmt.Fprintf(&b, "Mill-Base: %s\n", st.parentSHA)
	return b.String(), nil
}

// verifierSummary is the summary of the task's last verdict.
func verifierSummary(ctx context.Context, s *store.Store, id string) (string, error) {
	evs, err := s.TaskEvents(ctx, id)
	if err != nil {
		return "", err
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind != "task-verdict" {
			continue
		}
		var d struct {
			Verdict prompt.VerifierOutput `json:"verdict"`
		}
		if err := json.Unmarshal(evs[i].Data, &d); err != nil {
			return "", fmt.Errorf("corrupt task-verdict event %d: %w", evs[i].Seq, err)
		}
		return fmt.Sprintf("%s: %s", d.Verdict.Verdict, oneLine(d.Verdict.Summary)), nil
	}
	return "", errors.New("task " + id + " has no recorded verdict")
}

func nonEmpty(cmds []string) []string {
	var out []string
	for _, c := range cmds {
		if strings.TrimSpace(c) != "" {
			out = append(out, c)
		}
	}
	return out
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
