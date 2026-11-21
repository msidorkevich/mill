// Package taskspec parses and validates a single task: the same shape as one task in a
// plan (PL2), written by hand until the planner exists.
package taskspec

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Spec is one task.
type Spec struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Kind       string   `json:"kind"`
	Spec       string   `json:"spec"`
	Acceptance []string `json:"acceptance"`
	Checks     []string `json:"checks"`
	Owns       []string `json:"owns"`
	DependsOn  []string `json:"depends_on"`
	Size       string   `json:"size"`
}

// Kinds and sizes a task may have. Size L is rejected: such a task must be split (PL3).
var (
	Kinds = []string{"implement", "docs", "test"}
	Sizes = []string{"S", "M"}
)

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Load reads and validates a task file. Unknown fields are errors.
func Load(path string) (Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Spec{}, err
	}
	return Parse(data)
}

// Parse decodes and validates a task. A missing size defaults to "S".
func Parse(data []byte) (Spec, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var s Spec
	if err := dec.Decode(&s); err != nil {
		return Spec{}, fmt.Errorf("parse task: %w", err)
	}
	if dec.More() {
		return Spec{}, errors.New("parse task: trailing data after the task object")
	}
	s.ApplyDefaults()
	return s, s.Validate()
}

// ApplyDefaults sets size S when empty and turns nil lists into empty lists.
func (s *Spec) ApplyDefaults() {
	if s.Size == "" {
		s.Size = "S"
	}
	if s.Acceptance == nil {
		s.Acceptance = []string{}
	}
	if s.Checks == nil {
		s.Checks = []string{}
	}
	if s.Owns == nil {
		s.Owns = []string{}
	}
	if s.DependsOn == nil {
		s.DependsOn = []string{}
	}
}

// Validate reports every problem at once, including depends_on, which single tasks reject.
func (s Spec) Validate() error {
	var errs []error
	if err := s.ValidateFields(); err != nil {
		errs = append(errs, err)
	}
	if len(s.DependsOn) > 0 {
		errs = append(errs, errors.New("depends_on is not supported for single tasks: run dependent tasks one after another"))
	}
	return errors.Join(errs...)
}

// ValidateFields checks every field rule except depends_on.
func (s Spec) ValidateFields() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if !idRe.MatchString(s.ID) {
		add("id %q must be a lowercase slug: letters, digits and dashes, starting with a letter or digit, at most 63 characters", s.ID)
	}
	if strings.TrimSpace(s.Title) == "" {
		add("title must not be empty")
	}
	if !contains(Kinds, s.Kind) {
		add("kind must be one of %v, got %q", Kinds, s.Kind)
	}
	if strings.TrimSpace(s.Spec) == "" {
		add("spec must not be empty")
	}
	if s.Kind == "implement" && len(s.Acceptance) == 0 {
		add("an implement task needs at least one acceptance item")
	}
	for i, a := range s.Acceptance {
		if strings.TrimSpace(a) == "" {
			add("acceptance[%d] is empty", i)
		}
	}
	for i, c := range s.Checks {
		if strings.TrimSpace(c) == "" {
			add("checks[%d] is empty", i)
		}
	}
	if !contains(Sizes, s.Size) {
		if s.Size == "L" {
			add("size L is too big for one task: split it into S or M tasks")
		} else {
			add("size must be one of %v, got %q", Sizes, s.Size)
		}
	}
	return errors.Join(errs...)
}

func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
