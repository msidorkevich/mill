// Package cistatus decides when to ask the status hook about a published task, runs it and
// records what changed as ci-status events. It knows nothing about any forge: everything it
// learns comes from the hook's mill/status@1 document.
package cistatus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/msidorkevich/mill/internal/config"
	"github.com/msidorkevich/mill/internal/hook"
	"github.com/msidorkevich/mill/internal/store"
)

// Event is the event kind recorded when a task's CI state or published head changes.
const Event = "ci-status"

const publishedEvent = "task-published"

// Record is one ci-status event. LastQuery is when the status hook last ran for the task
// (successfully or not): the newer of At and the task's latest status hook-run event.
type Record struct {
	Seq       int64              `json:"seq"`
	At        time.Time          `json:"at"`
	SHA       string             `json:"sha"`
	State     string             `json:"state"`
	URL       string             `json:"url,omitempty"`
	Checks    []hook.StatusCheck `json:"checks"`
	LastQuery time.Time          `json:"-"`
}

// Change is a ci-status event Poll appended. Previous is the state recorded before it ("" when
// there was none).
type Change struct {
	TaskID   string             `json:"task_id"`
	SHA      string             `json:"sha"`
	State    string             `json:"state"`
	URL      string             `json:"url,omitempty"`
	Checks   []hook.StatusCheck `json:"checks"`
	Previous string             `json:"previous"`
}

// Options configure Poll.
type Options struct {
	Store *store.Store
	Cfg   config.Config
	Root  string // the repository root the hook runs in
	Goal  string // when set, only tasks of this goal are polled
	// Out gets one line per change; Stderr gets hook failures.
	Out, Stderr io.Writer
	// Now is the clock. Defaults to time.Now. Event times come from the store's clock, so
	// tests give both the same clock.
	Now func() time.Time
}

// Latest reads the newest ci-status event of taskID. The bool is false when there is none;
// LastQuery is set from the task's latest status hook-run either way.
func Latest(ctx context.Context, s *store.Store, taskID string) (Record, bool, error) {
	evs, err := s.TaskEvents(ctx, taskID)
	if err != nil {
		return Record{}, false, fmt.Errorf("read the events of %s for its CI status: %w", taskID, err)
	}
	return latest(evs, taskID)
}

func latest(evs []store.Event, taskID string) (Record, bool, error) {
	var rec Record
	var found, ran bool
	var ranAt time.Time
	for i := len(evs) - 1; i >= 0 && !(found && ran); i-- {
		e := evs[i]
		switch {
		case e.Kind == Event && !found:
			var d struct {
				SHA    string             `json:"sha"`
				State  string             `json:"state"`
				URL    string             `json:"url"`
				Checks []hook.StatusCheck `json:"checks"`
			}
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return Record{}, false, fmt.Errorf("corrupt %s event %d of %s: %w; check the mill database", Event, e.Seq, taskID, err)
			}
			rec = Record{Seq: e.Seq, At: e.At, SHA: d.SHA, State: d.State, URL: d.URL, Checks: d.Checks}
			found = true
		case e.Kind == hook.RunEvent && !ran:
			var d struct {
				Event string `json:"event"`
			}
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return Record{}, false, fmt.Errorf("corrupt %s event %d of %s: %w; check the mill database", hook.RunEvent, e.Seq, taskID, err)
			}
			if d.Event == hook.EventStatus {
				ranAt, ran = e.At, true
			}
		}
	}
	rec.LastQuery = rec.At
	if ran && ranAt.After(rec.LastQuery) {
		rec.LastQuery = ranAt
	}
	return rec, found, nil
}

// Due reports whether the status hook should run for a task whose published head is
// publishedSHA: nothing is recorded, the record is of another head, its state is not final
// (success or failure), or the last query is older than interval.
func Due(rec Record, found bool, publishedSHA string, now time.Time, interval time.Duration) bool {
	if !found || rec.SHA != publishedSHA || !final(rec.State) {
		return true
	}
	last := rec.LastQuery
	if rec.At.After(last) {
		last = rec.At
	}
	return now.Sub(last) >= interval
}

func final(state string) bool { return state == "success" || state == "failure" }

// Display is the CI column for a task: "-" when nothing is recorded, the state when the
// record is of the published head, otherwise "stale (<sha8>)".
func Display(rec Record, found bool, publishedSHA string) string {
	switch {
	case !found:
		return "-"
	case rec.SHA == publishedSHA:
		return rec.State
	}
	return fmt.Sprintf("stale (%s)", short(rec.SHA))
}

// FailingChecks returns the checks of rec whose state is failure.
func FailingChecks(rec Record) []hook.StatusCheck {
	var out []hook.StatusCheck
	for _, c := range rec.Checks {
		if c.State == "failure" {
			out = append(out, c)
		}
	}
	return out
}

// Poll runs the status hook for every due published task (of o.Goal when set), in publish
// order, and records each changed state or head as a ci-status event, printing one line per
// change to o.Out. A failing hook is reported on o.Stderr and recorded by the hook runner;
// it changes nothing and Poll goes on with the next task. With no status hook configured
// Poll does nothing.
func Poll(ctx context.Context, o Options) ([]Change, error) {
	if o.Cfg.Status == "" {
		return nil, nil
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	out, stderr := o.Out, o.Stderr
	if out == nil {
		out = io.Discard
	}
	ids, err := publishedTasks(ctx, o.Store, o.Goal)
	if err != nil {
		return nil, err
	}
	runner := hook.Runner{Store: o.Store, Cfg: o.Cfg, Root: o.Root, Stderr: stderr, Now: now}
	var changes []Change
	for _, id := range ids {
		br, err := o.Store.GetBranch(ctx, id)
		if err != nil {
			return changes, fmt.Errorf("task %s is published but its branch cannot be loaded: %w", id, err)
		}
		rec, found, err := Latest(ctx, o.Store, id)
		if err != nil {
			return changes, err
		}
		if !Due(rec, found, br.PublishedSHA, now(), o.Cfg.StatusInterval.Duration) {
			continue
		}
		doc, err := hook.Build(ctx, o.Store, o.Cfg, o.Root, id, hook.EventStatus, "")
		if err != nil {
			return changes, fmt.Errorf("query the CI status of %s: %w", id, err)
		}
		goalID, _ := store.SplitTaskID(id)
		res, err := runner.Run(ctx, hook.EventStatus, goalID, id, doc)
		if err != nil {
			return changes, fmt.Errorf("query the CI status of %s: %w", id, err)
		}
		if res.Skipped || res.Failed() || res.Status == nil {
			continue
		}
		st := res.Status
		if found && rec.State == st.State && rec.SHA == doc.HeadSHA {
			continue
		}
		checks := st.Checks
		if checks == nil {
			checks = []hook.StatusCheck{}
		}
		c := Change{TaskID: id, SHA: doc.HeadSHA, State: st.State, URL: st.URL, Checks: checks}
		if found {
			c.Previous = rec.State
		}
		if _, err := o.Store.AppendEvent(ctx, Event, goalID, id,
			map[string]any{"sha": c.SHA, "state": c.State, "url": c.URL, "checks": c.Checks}); err != nil {
			return changes, fmt.Errorf("record the CI status %s of %s: %w", c.State, id, err)
		}
		changes = append(changes, c)
		fmt.Fprintln(out, line(c))
	}
	return changes, nil
}

// line is "<task>: CI <state> at <sha8>[: <failing checks>][ <url>]".
func line(c Change) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: CI %s at %s", c.TaskID, c.State, short(c.SHA))
	var names []string
	for _, ch := range FailingChecks(Record{Checks: c.Checks}) {
		names = append(names, ch.Name)
	}
	if len(names) > 0 {
		b.WriteString(": " + strings.Join(names, ", "))
	}
	if c.URL != "" {
		b.WriteString(" " + c.URL)
	}
	return b.String()
}

// publishedTasks lists the published (so not landed) tasks, of goal when set, ordered by
// their first task-published event.
func publishedTasks(ctx context.Context, s *store.Store, goal string) ([]string, error) {
	tasks, err := s.TaskSummaries(ctx, store.TaskPublished)
	if err != nil {
		return nil, fmt.Errorf("list the published tasks: %w", err)
	}
	type entry struct {
		id  string
		seq int64
	}
	var es []entry
	for _, t := range tasks {
		if g, _ := store.SplitTaskID(t.ID); goal != "" && g != goal {
			continue
		}
		evs, err := s.TaskEvents(ctx, t.ID)
		if err != nil {
			return nil, fmt.Errorf("read the events of %s to order the published tasks: %w", t.ID, err)
		}
		seq := int64(-1)
		for _, e := range evs {
			if e.Kind == publishedEvent {
				seq = e.Seq
				break
			}
		}
		if seq < 0 {
			return nil, fmt.Errorf("task %s is published but has no %s event; publish it again with `mill review approve %s`", t.ID, publishedEvent, t.ID)
		}
		es = append(es, entry{t.ID, seq})
	}
	sort.Slice(es, func(i, j int) bool { return es[i].seq < es[j].seq })
	ids := make([]string, len(es))
	for i, e := range es {
		ids[i] = e.id
	}
	return ids, nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
