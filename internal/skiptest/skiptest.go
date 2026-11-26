// Package skiptest finds the test skips (t.Skip, t.Skipf, t.SkipNow) a change adds, so a
// task that silences a test instead of fixing it is on record.
package skiptest

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/msidorkevich/mill/internal/gitx"
)

// Skip is one added line that skips a test.
type Skip struct {
	File   string `json:"file"`
	Line   int    `json:"line"`   // line number in the new file
	Reason string `json:"reason"` // the first argument when it is a string literal, unquoted
}

// skipCall matches a Skip, Skipf or SkipNow call on any receiver identifier.
var skipCall = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*\.(SkipNow|Skipf|Skip)\s*\(`)

// hunkHeader captures the new-file start line of a hunk.
var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// Added returns every line added to a *_test.go file between startSHA and head that calls
// Skip, Skipf or SkipNow, in diff order.
func Added(ctx context.Context, dir, startSHA, head string) ([]Skip, error) {
	out, err := gitx.Run(ctx, dir, "-c", "core.quotePath=false", "diff", "--no-color", "--no-ext-diff",
		"-U0", "-M", "--src-prefix=a/", "--dst-prefix=b/", startSHA+".."+head, "--", "*_test.go")
	if err != nil {
		return nil, fmt.Errorf("diff test files %s..%s: %w", short(startSHA), short(head), err)
	}
	return parse(out)
}

// parse reads a -U0 unified diff and returns the added skip calls.
func parse(diff string) ([]Skip, error) {
	var skips []Skip
	file, line, inHunk := "", 0, false
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			file, inHunk = "", false
		case !inHunk && strings.HasPrefix(l, "+++ "):
			name := strings.TrimPrefix(l, "+++ ")
			if name == "/dev/null" {
				file = ""
			} else {
				file = strings.TrimPrefix(name, "b/")
			}
		case strings.HasPrefix(l, "@@ "):
			m := hunkHeader.FindStringSubmatch(l)
			if m == nil {
				return nil, fmt.Errorf("unreadable hunk header %q in the test diff", l)
			}
			n, err := strconv.Atoi(m[1])
			if err != nil {
				return nil, fmt.Errorf("unreadable hunk header %q in the test diff: %w", l, err)
			}
			line, inHunk = n, true
		case inHunk && strings.HasPrefix(l, "+"):
			if file != "" && strings.HasSuffix(file, "_test.go") {
				skips = append(skips, skipsIn(file, line, l[1:])...)
			}
			line++
		}
	}
	return skips, nil
}

// skipsIn returns the skip calls on one source line. Comment-only lines are ignored.
func skipsIn(file string, line int, src string) []Skip {
	if strings.HasPrefix(strings.TrimSpace(src), "//") {
		return nil
	}
	var skips []Skip
	for _, m := range skipCall.FindAllStringSubmatchIndex(src, -1) {
		reason := ""
		if src[m[2]:m[3]] != "SkipNow" {
			reason = literal(src[m[1]:])
		}
		skips = append(skips, Skip{File: file, Line: line, Reason: reason})
	}
	return skips
}

// literal returns the unquoted string literal at the start of s (after spaces), or "".
func literal(s string) string {
	s = strings.TrimLeft(s, " \t")
	q, err := strconv.QuotedPrefix(s)
	if err != nil || strings.HasPrefix(q, "'") {
		return ""
	}
	v, err := strconv.Unquote(q)
	if err != nil {
		return ""
	}
	return v
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
