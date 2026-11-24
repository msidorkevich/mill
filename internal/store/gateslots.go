package store

import (
	"context"
	"fmt"
	"time"
)

// GateHolder identifies who holds a gate slot: the task attempt running its gate and the
// mill process running it.
type GateHolder struct {
	TaskID  string
	Attempt int
	PID     int
}

// AcquireGateSlot takes one of max gate slots shared by every mill process on this
// database. When all slots are held it calls onWait once (if not nil) and polls every
// poll until a slot frees up or ctx is done. Slots held by a process that is no longer
// alive are reclaimed. The returned release frees the slot; call it once the gate is done.
func (s *Store) AcquireGateSlot(ctx context.Context, h GateHolder, max int, poll time.Duration, onWait func()) (release func() error, err error) {
	if max < 1 {
		return nil, fmt.Errorf("acquire gate slot for %s: max_parallel_gates must be >= 1, got %d", h.TaskID, max)
	}
	if poll <= 0 {
		return nil, fmt.Errorf("acquire gate slot for %s: poll interval must be > 0, got %s", h.TaskID, poll)
	}
	waited := false
	for {
		id, ok, err := s.tryGateSlot(ctx, h, max)
		if err != nil {
			return nil, fmt.Errorf("acquire gate slot for %s: %w", h.TaskID, err)
		}
		if ok {
			return s.gateSlotRelease(ctx, h, id), nil
		}
		if !waited {
			waited = true
			if onWait != nil {
				onWait()
			}
		}
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, fmt.Errorf("acquire gate slot for %s: stopped waiting for a free slot (%d in use): %w", h.TaskID, max, ctx.Err())
		case <-t.C:
		}
	}
}

// tryGateSlot reclaims slots of dead processes and takes a free slot if there is one, in
// one transaction. ok is false when every slot is held.
func (s *Store) tryGateSlot(ctx context.Context, h GateHolder, max int) (id int64, ok bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT id, task_id, pid FROM gate_slots ORDER BY id`)
	if err != nil {
		return 0, false, fmt.Errorf("list gate slots: %w", err)
	}
	type slot struct {
		id     int64
		taskID string
		pid    int
	}
	var held []slot
	for rows.Next() {
		var sl slot
		if err := rows.Scan(&sl.id, &sl.taskID, &sl.pid); err != nil {
			rows.Close()
			return 0, false, fmt.Errorf("list gate slots: %w", err)
		}
		held = append(held, sl)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, false, fmt.Errorf("list gate slots: %w", err)
	}
	rows.Close()

	inUse := 0
	for _, sl := range held {
		alive, err := s.alive(TaskClaim{TaskID: sl.taskID, PID: sl.pid})
		if err != nil {
			return 0, false, fmt.Errorf("check gate slot of pid %d on %s: %w", sl.pid, sl.taskID, err)
		}
		if alive {
			inUse++
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM gate_slots WHERE id = ?`, sl.id); err != nil {
			return 0, false, fmt.Errorf("reclaim gate slot of dead pid %d on %s: %w", sl.pid, sl.taskID, err)
		}
	}
	if inUse >= max {
		// Commit anyway so reclaimed slots stay reclaimed.
		if err := tx.Commit(); err != nil {
			return 0, false, fmt.Errorf("commit gate slot reclaim: %w", err)
		}
		return 0, false, nil
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO gate_slots (task_id, attempt, pid, acquired_at) VALUES (?, ?, ?, ?)`,
		h.TaskID, h.Attempt, h.PID, s.ts())
	if err != nil {
		return 0, false, fmt.Errorf("insert gate slot: %w", err)
	}
	id, err = res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("insert gate slot: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("commit gate slot: %w", err)
	}
	return id, true, nil
}

// gateSlotRelease returns the release func for slot id. It runs even after ctx is
// cancelled, so a gate interrupted by cancellation still frees its slot.
func (s *Store) gateSlotRelease(ctx context.Context, h GateHolder, id int64) func() error {
	ctx = context.WithoutCancel(ctx)
	return func() error {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM gate_slots WHERE id = ?`, id); err != nil {
			return fmt.Errorf("release gate slot of %s attempt %d: %w; it is reclaimed once pid %d exits", h.TaskID, h.Attempt, err, h.PID)
		}
		return nil
	}
}
