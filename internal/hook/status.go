package hook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

// StatusSchema is the version of the document a status hook prints on stdout.
const StatusSchema = "mill/status@1"

// maxSummary bounds a check's summary; a longer one is cut.
const maxSummary = 2048

// StatusStates are the states of a review and of each of its checks.
var StatusStates = []string{"success", "pending", "failure", "none"}

// Status is the review status a status hook reports for a published task.
type Status struct {
	Schema string        `json:"schema"`
	State  string        `json:"state"`
	URL    string        `json:"url,omitempty"`
	Checks []StatusCheck `json:"checks"`
}

// StatusCheck is one check of a review, such as a CI job. (It is not named Check because
// the package already has the Check function.)
type StatusCheck struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	URL     string `json:"url,omitempty"`
	Summary string `json:"summary"`
}

// ParseStatus reads the last JSON object in stdout (one that starts a line) as a
// mill/status@1 document and validates it. A check summary longer than 2048 bytes is cut.
func ParseStatus(stdout []byte) (Status, error) {
	raw, ok := lastObject(stdout)
	if !ok {
		return Status{}, errors.New("stdout has no JSON object")
	}
	var st Status
	if err := json.Unmarshal(raw, &st); err != nil {
		return Status{}, fmt.Errorf("decode %s document: %w", StatusSchema, err)
	}
	if st.Schema != StatusSchema {
		return Status{}, fmt.Errorf("schema is %q, want %q", st.Schema, StatusSchema)
	}
	if !validState(st.State) {
		return Status{}, fmt.Errorf("state is %q, want one of %v", st.State, StatusStates)
	}
	for i := range st.Checks {
		c := &st.Checks[i]
		if c.Name == "" {
			return Status{}, fmt.Errorf("check %d has no name", i+1)
		}
		if !validState(c.State) {
			return Status{}, fmt.Errorf("check %q has state %q, want one of %v", c.Name, c.State, StatusStates)
		}
		c.Summary = cut(c.Summary, maxSummary)
	}
	return st, nil
}

// lastObject returns the last top-level JSON object in b that starts at the beginning of a
// line (after optional indentation).
func lastObject(b []byte) (json.RawMessage, bool) {
	var last json.RawMessage
	for pos := 0; pos < len(b); {
		end := bytes.IndexByte(b[pos:], '\n')
		next := len(b)
		if end >= 0 {
			next = pos + end + 1
		}
		start := pos + len(b[pos:next]) - len(bytes.TrimLeft(b[pos:next], " \t\r"))
		if start < next && b[start] == '{' {
			dec := json.NewDecoder(bytes.NewReader(b[start:]))
			var v json.RawMessage
			if err := dec.Decode(&v); err == nil {
				last = v
				pos = start + int(dec.InputOffset())
				continue
			}
		}
		pos = next
	}
	return last, last != nil
}

func validState(s string) bool {
	for _, v := range StatusStates {
		if s == v {
			return true
		}
	}
	return false
}

// cut returns s cut to at most n bytes without splitting a UTF-8 sequence.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
