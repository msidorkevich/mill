package cistatus

import (
	"context"
	"fmt"

	"github.com/msidorkevich/mill/internal/hook"
	"github.com/msidorkevich/mill/internal/store"
)

// RecordIfChanged records st, the status a status hook reported for taskID at published head
// sha, as a ci-status event when its state or sha differs from the task's latest record. It
// reports whether it recorded an event; an unchanged status records nothing.
func RecordIfChanged(ctx context.Context, s *store.Store, taskID, sha string, st hook.Status) (bool, error) {
	rec, found, err := Latest(ctx, s, taskID)
	if err != nil {
		return false, err
	}
	if found && rec.State == st.State && rec.SHA == sha {
		return false, nil
	}
	checks := st.Checks
	if checks == nil {
		checks = []hook.StatusCheck{}
	}
	goalID, _ := store.SplitTaskID(taskID)
	if _, err := s.AppendEvent(ctx, Event, goalID, taskID,
		map[string]any{"sha": sha, "state": st.State, "url": st.URL, "checks": checks}); err != nil {
		return false, fmt.Errorf("record the CI status %s of %s: %w", st.State, taskID, err)
	}
	return true, nil
}
