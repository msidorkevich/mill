package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/gitx"
	"github.com/msidorkevich/mill/internal/keeper"
	"github.com/msidorkevich/mill/internal/project"
	"github.com/msidorkevich/mill/internal/store"
)

// combinedHoldBack runs the combined check over the verified (not yet published) tasks among
// ids, the goal's tasks in review order, before review approve --goal publishes any of them.
// While the check is red it sets aside the pair it reports (or the culprit alone) and checks
// the rest again, until it is green or fewer than two tasks remain. It returns the tasks set
// aside, after printing why for each pair. Combined releases its claims before it returns,
// so the caller holds none of them when it takes its publish claims. An error means the check
// could not decide; nothing should be published then.
func combinedHoldBack(ctx context.Context, env Env, paths project.Paths, cfg config.Config, s *store.Store, goalID string, ids []string) (held []string, err error) {
	tasks, err := s.GoalTasks(ctx, goalID)
	if err != nil {
		return nil, err
	}
	verified := map[string]bool{}
	for _, t := range tasks {
		verified[t.ID] = t.Status == store.TaskVerified
	}
	var set []string
	for _, id := range ids {
		if verified[id] {
			set = append(set, id)
		}
	}
	if len(set) < 2 {
		return nil, nil
	}
	skip := "; nothing was published (skip the check with --no-combined)"
	if err := gitx.Fetch(ctx, paths.Root, cfg.Remote); err != nil {
		return nil, fmt.Errorf("combined check: fetch %s: %w%s", cfg.Remote, err, skip)
	}
	out := &lockedWriter{w: env.Stdout}
	p, err := newPipeline(env, "review approve", paths, cfg, "", s, out)
	if err != nil {
		return nil, fmt.Errorf("combined check: %w%s", err, skip)
	}
	p.MaxParallelGates = cfg.MaxParallelGates
	claimer := store.NewClaimer("review approve")
	for len(set) >= 2 {
		res, err := keeper.Combined(ctx, keeper.Options{
			Store: s, Cfg: cfg, Paths: paths, Host: p.Host, Pipeline: p, Goal: goalID, Tasks: set,
			Out: out, Claimer: claimer, MaxParallelGates: cfg.MaxParallelGates, GateSlotPoll: approveGateSlotPoll,
		})
		if err != nil {
			return nil, fmt.Errorf("%w%s", err, skip)
		}
		if !res.Ran || res.Passed {
			return held, nil
		}
		pair := []string{res.Culprit}
		if res.Partner != "" {
			pair = append(pair, res.Partner)
		}
		if res.Culprit == "" || slices.ContainsFunc(pair, func(id string) bool { return !slices.Contains(set, id) }) {
			return nil, fmt.Errorf("combined check over %s is red but names no culprit among them (culprit %q, partner %q): %s%s",
				strings.Join(set, ", "), res.Culprit, res.Partner, strings.TrimSpace(res.Failed), skip)
		}
		failed := strings.TrimSpace(res.Failed)
		if res.Partner != "" {
			fmt.Fprintf(env.Stderr, "combined check: %s conflicts with %s: %s; not publishing them. Fix one and run `mill review approve` again\n",
				res.Culprit, res.Partner, failed)
		} else {
			fmt.Fprintf(env.Stderr, "combined check: %s fails: %s; not publishing it. Fix it and run `mill review approve` again\n",
				res.Culprit, failed)
		}
		held = append(held, pair...)
		set = slices.DeleteFunc(set, func(id string) bool { return slices.Contains(pair, id) })
	}
	return held, nil
}
