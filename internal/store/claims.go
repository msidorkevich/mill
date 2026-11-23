package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Claimer is a mill command that claims tasks while it works on them. Claims are
// short-lived: the command releases them when it is done, and a claim whose process has
// died is taken over.
type Claimer struct {
	Owner      string // unique per claimer: pid and a random nonce
	PID        int
	Command    string // mill subcommand, e.g. "run"
	Executable string // argv[0] of the claiming process, to tell a reused pid apart
}

// NewClaimer returns a Claimer for this process running the mill subcommand command.
func NewClaimer(command string) Claimer {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on supported platforms; fall back to the clock.
		copy(b[:], strconv.FormatInt(time.Now().UnixNano(), 16))
	}
	pid := os.Getpid()
	exe := ""
	if len(os.Args) > 0 {
		exe = os.Args[0]
	}
	return Claimer{
		Owner:      fmt.Sprintf("%d-%s", pid, hex.EncodeToString(b[:])),
		PID:        pid,
		Command:    command,
		Executable: exe,
	}
}

// TaskClaim is a stored claim on a task.
type TaskClaim struct {
	TaskID     string    `json:"task_id"`
	Owner      string    `json:"owner"`
	PID        int       `json:"pid"`
	Command    string    `json:"command"`
	Executable string    `json:"executable,omitempty"`
	ClaimedAt  time.Time `json:"claimed_at"`
}

// BusyError is returned by ClaimTasks when a task is already being worked on.
type BusyError struct {
	TaskID string
	Doing  string // e.g. "attempt 2 (implementer) running", "running (gate)", "claimed by mill run (pid 42)"
}

func (e *BusyError) Error() string {
	return fmt.Sprintf("task %s is busy: %s; retry when it finishes", e.TaskID, e.Doing)
}

// ClaimTasks claims every task in ids for c, or none of them. A task is busy when its
// status is running, when it has a running attempt, or when another owner's live process
// holds a claim on it; ClaimTasks then returns a *BusyError and claims nothing. A claim
// held by a dead process is replaced.
func (s *Store) ClaimTasks(ctx context.Context, c Claimer, ids []string) error {
	if c.Owner == "" {
		return errors.New("claim tasks: claimer has no owner; build it with NewClaimer")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("claim tasks: %w", err)
	}
	defer tx.Rollback()
	now := s.ts()
	for _, id := range ids {
		var status, phase string
		err := tx.QueryRowContext(ctx, `SELECT status, phase FROM tasks WHERE id = ?`, id).Scan(&status, &phase)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("claim task %s: %w", id, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("claim task %s: %w", id, err)
		}
		var n int
		var role string
		err = tx.QueryRowContext(ctx, `SELECT n, role FROM attempts WHERE task_id = ? AND status = ?
			ORDER BY id DESC LIMIT 1`, id, AttemptRunning).Scan(&n, &role)
		switch {
		case err == nil:
			return &BusyError{TaskID: id, Doing: fmt.Sprintf("attempt %d (%s) running", n, role)}
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("claim task %s: %w", id, err)
		}
		if status == TaskRunning {
			doing := "running"
			if phase != "" {
				doing += " (" + phase + ")"
			}
			return &BusyError{TaskID: id, Doing: doing}
		}
		held, err := scanClaim(tx.QueryRowContext(ctx, `SELECT task_id, owner, pid, command, executable, claimed_at
			FROM task_claims WHERE task_id = ?`, id))
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("claim task %s: %w", id, err)
		case held.Owner != c.Owner:
			alive, err := s.alive(held)
			if err != nil {
				return fmt.Errorf("claim task %s: check claim of pid %d: %w", id, held.PID, err)
			}
			if alive {
				return &BusyError{TaskID: id, Doing: fmt.Sprintf("claimed by mill %s (pid %d)", held.Command, held.PID)}
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_claims (task_id, owner, pid, command, executable, claimed_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (task_id) DO UPDATE SET owner = excluded.owner, pid = excluded.pid,
			command = excluded.command, executable = excluded.executable, claimed_at = excluded.claimed_at`,
			id, c.Owner, c.PID, c.Command, c.Executable, now); err != nil {
			return fmt.Errorf("claim task %s: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("claim tasks: %w", err)
	}
	return nil
}

// ReleaseClaims deletes c's claims on ids. Claims held by other owners are left alone.
func (s *Store) ReleaseClaims(ctx context.Context, c Claimer, ids []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("release claims: %w", err)
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM task_claims WHERE task_id = ? AND owner = ?`, id, c.Owner); err != nil {
			return fmt.Errorf("release claim on %s: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("release claims: %w", err)
	}
	return nil
}

// ClaimedTasks lists the claims whose process is still alive, by task id.
func (s *Store) ClaimedTasks(ctx context.Context) (map[string]TaskClaim, error) {
	claims, err := s.allClaims(ctx, s.db)
	if err != nil {
		return nil, fmt.Errorf("list task claims: %w", err)
	}
	out := map[string]TaskClaim{}
	for _, c := range claims {
		alive, err := s.alive(c)
		if err != nil {
			return nil, fmt.Errorf("list task claims: check claim of pid %d on %s: %w", c.PID, c.TaskID, err)
		}
		if alive {
			out[c.TaskID] = c
		}
	}
	return out, nil
}

type queryer interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}

func (s *Store) allClaims(ctx context.Context, q queryer) ([]TaskClaim, error) {
	rows, err := q.QueryContext(ctx, `SELECT task_id, owner, pid, command, executable, claimed_at
		FROM task_claims ORDER BY task_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskClaim
	for rows.Next() {
		c, err := scanClaim(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// deleteDeadClaimsTx removes the claims whose process has died.
func (s *Store) deleteDeadClaimsTx(ctx context.Context, tx *sql.Tx) error {
	claims, err := s.allClaims(ctx, tx)
	if err != nil {
		return fmt.Errorf("list task claims: %w", err)
	}
	for _, c := range claims {
		alive, err := s.alive(c)
		if err != nil {
			return fmt.Errorf("check claim of pid %d on %s: %w", c.PID, c.TaskID, err)
		}
		if alive {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM task_claims WHERE task_id = ? AND owner = ?`, c.TaskID, c.Owner); err != nil {
			return fmt.Errorf("delete dead claim on %s: %w", c.TaskID, err)
		}
	}
	return nil
}

func scanClaim(r rowScanner) (TaskClaim, error) {
	var c TaskClaim
	var at string
	if err := r.Scan(&c.TaskID, &c.Owner, &c.PID, &c.Command, &c.Executable, &at); err != nil {
		return TaskClaim{}, err
	}
	c.ClaimedAt, _ = parseTime(at)
	return c, nil
}

// claimProcessAlive is the default liveness check: the pid exists and still runs the
// claiming mill command. A pid reused by an unrelated process counts as dead.
func claimProcessAlive(c TaskClaim) (bool, error) {
	if c.PID <= 0 {
		return false, nil
	}
	if err := syscall.Kill(c.PID, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		if !errors.Is(err, syscall.EPERM) {
			return false, fmt.Errorf("kill -0 %d: %w", c.PID, err)
		}
	}
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(c.PID)).Output()
	line := strings.TrimSpace(string(out))
	if err != nil {
		var ee *exec.ExitError
		// ps exits 1 with no output when the pid does not exist.
		if errors.As(err, &ee) && ee.ExitCode() == 1 && line == "" {
			return false, nil
		}
		return false, fmt.Errorf("ps -p %d: %w", c.PID, err)
	}
	if line == "" {
		return false, nil
	}
	return runsClaimer(line, c.Executable, c.Command), nil
}

// runsClaimer reports whether the command line cmdline is exe running the mill subcommand
// command.
func runsClaimer(cmdline, exe, command string) bool {
	if exe != "" && cmdline != exe && !strings.HasPrefix(cmdline, exe+" ") {
		return false
	}
	if command == "" {
		return true
	}
	return strings.Contains(" "+cmdline+" ", " "+command+" ")
}
