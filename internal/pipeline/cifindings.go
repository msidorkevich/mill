package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/msidorkevich/mill/internal/prompt"
)

// CICheck is one check of a CI run as the status hook reports it. It has the fields of
// hook.StatusCheck, which converts to it (pipeline cannot import hook: hook imports pipeline).
type CICheck struct {
	Name    string
	State   string
	URL     string
	Summary string
}

// RecordCIFailure records the failing checks of a CI run (as reported by the status hook) as
// the findings of a new attempt number, so the next Run starts its implementer with them.
// Checks whose state is not failure are left out; with no failing check it fails.
func (p *Pipeline) RecordCIFailure(ctx context.Context, taskID string, checks []CICheck) error {
	var findings []prompt.Finding
	for _, c := range checks {
		if c.State == "failure" {
			findings = append(findings, ciFinding(c))
		}
	}
	if len(findings) == 0 {
		return fmt.Errorf("CI result for %s has no failing check to record", taskID)
	}
	prev, err := p.Store.MaxAttemptN(ctx, taskID)
	if err != nil {
		return err
	}
	return p.recordFindings(ctx, taskID, prev+1, findings)
}

// ciFinding describes one failing CI check.
func ciFinding(c CICheck) prompt.Finding {
	issue := fmt.Sprintf("CI check `%s` failed", c.Name)
	if c.URL != "" {
		issue += " (" + c.URL + ")"
	}
	if s := strings.TrimSpace(c.Summary); s != "" {
		issue += ". Summary:\n" + indent(s)
	}
	return prompt.Finding{Source: "ci", Issue: issue}
}
