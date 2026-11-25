package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func lastBlockStore(t *testing.T) *Store {
	t.Helper()
	s := open(t, filepath.Join(t.TempDir(), "mill.db"))
	if _, err := s.DB().ExecContext(context.Background(), `INSERT INTO goals (id, title, status, created_at, updated_at)
		VALUES ('G-1', 'g', 'running', '2026-01-01T00:00:00.000000000Z', '2026-01-01T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(context.Background(), `INSERT INTO tasks (id, goal_id, title, kind, spec, status, created_at, updated_at)
		VALUES ('t1', 'G-1', 'T', 'implement', 's', 'pending', '2026-01-01T00:00:00.000000000Z', '2026-01-01T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	return s
}

func lastBlockSet(t *testing.T, s *Store, status, phase, reason string) {
	t.Helper()
	if err := s.SetTaskStatus(context.Background(), "t1", status, phase, reason); err != nil {
		t.Fatalf("SetTaskStatus(%s): %v", status, err)
	}
}

func assertLastBlock(t *testing.T, s *Store, phase, reason string) {
	t.Helper()
	p, r, err := s.LastBlockedPhase(context.Background(), "t1")
	if err != nil || p != phase || r != reason {
		t.Fatalf("LastBlockedPhase = %q, %q, %v; want %q, %q, nil", p, r, err, phase, reason)
	}
}

func TestLastBlockedPhaseSurvivesResetToPending(t *testing.T) {
	t.Parallel()
	s := lastBlockStore(t)
	lastBlockSet(t, s, TaskBlocked, PhaseVerify, "verifier failed")
	ids, err := s.ResetGoalTasks(context.Background(), "G-1", []string{TaskBlocked}, "", "retry")
	if err != nil || len(ids) != 1 {
		t.Fatalf("ResetGoalTasks = %v, %v; want t1 reset", ids, err)
	}
	lastBlockSet(t, s, TaskRunning, PhaseImplement, "")
	assertLastBlock(t, s, PhaseVerify, "verifier failed")
}

func TestLastBlockedPhaseReturnsTheMostRecentBlock(t *testing.T) {
	t.Parallel()
	s := lastBlockStore(t)
	lastBlockSet(t, s, TaskBlocked, PhaseImplement, "first")
	lastBlockSet(t, s, TaskPending, "", "retry")
	lastBlockSet(t, s, TaskBlocked, PhaseVerify, "second")
	assertLastBlock(t, s, PhaseVerify, "second")
}

func TestLastBlockedPhaseIsClearedWhenTaskLaterFinishes(t *testing.T) {
	t.Parallel()
	for _, done := range []string{TaskVerified, TaskPublished, TaskLanded} {
		s := lastBlockStore(t)
		lastBlockSet(t, s, TaskBlocked, PhaseVerify, "r")
		lastBlockSet(t, s, done, "", "")
		lastBlockSet(t, s, TaskPending, "", "changes requested")
		assertLastBlock(t, s, "", "")
	}
}

func TestLastBlockedPhaseIsEmptyForTaskNeverBlocked(t *testing.T) {
	t.Parallel()
	s := lastBlockStore(t)
	assertLastBlock(t, s, "", "")
	lastBlockSet(t, s, TaskRunning, PhaseImplement, "")
	assertLastBlock(t, s, "", "")
}

func TestLastBlockedPhaseFailsOnCorruptEventNamingItsSeq(t *testing.T) {
	t.Parallel()
	for _, data := range []string{"not json", "{}"} {
		s := lastBlockStore(t)
		lastBlockSet(t, s, TaskBlocked, PhaseVerify, "r")
		res, err := s.DB().ExecContext(context.Background(), `INSERT INTO events (at, kind, task_id, data)
			VALUES ('2026-01-01T00:00:00.000000000Z', 'task-status', 't1', ?)`, data)
		if err != nil {
			t.Fatal(err)
		}
		seq, err := res.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = s.LastBlockedPhase(context.Background(), "t1")
		if want := fmt.Sprintf("event %d", seq); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("payload %q: error = %v; want it to name %q", data, err, want)
		}
	}
}
