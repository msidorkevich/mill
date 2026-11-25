package gate

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
)

var (
	goFailedTest = regexp.MustCompile(`^\s*--- FAIL: (\S+) \(`)
	goFailedPkg  = regexp.MustCompile(`^FAIL\t(\S+)`)
	goPassedPkg  = regexp.MustCompile(`^ok\s*\t(\S+)`)
)

const (
	goPkgKey = "pkg:" // marks a package that failed without a failing test
	// goPkgContext is how many lines before a package's FAIL line make up its excerpt.
	goPkgContext = 20
)

// GoTestFailures returns the failing tests in go test output, sorted and without duplicates.
// A package that failed with no failing test of its own (build failure, panic) is returned
// as "pkg:<package>". ok is false when the output has no go test failure lines.
func GoTestFailures(output string) (failed []string, ok bool) {
	tests := 0 // failing tests seen since the last package result line
	for _, line := range strings.Split(output, "\n") {
		if m := goFailedTest.FindStringSubmatch(line); m != nil {
			failed = append(failed, m[1])
			tests++
		} else if m := goFailedPkg.FindStringSubmatch(line); m != nil {
			if tests == 0 {
				failed = append(failed, goPkgKey+m[1])
			}
			tests = 0
		} else if goPassedPkg.MatchString(line) {
			tests = 0
		}
	}
	slices.Sort(failed)
	return slices.Compact(failed), len(failed) > 0
}

// GoTestFailuresFromLog is GoTestFailures applied to the contents of a log file.
func GoTestFailuresFromLog(path string) ([]string, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("reading go test log: %w; check that the log exists and is readable, or run the gate again", err)
	}
	failed, ok := GoTestFailures(string(b))
	return failed, ok, nil
}

// GoTestExcerpt returns the output belonging to each of names, which are keys as returned by
// GoTestFailures. The result has at most maxLines lines; when lines are cut, the last one
// says how many.
func GoTestExcerpt(output string, names []string, maxLines int) string {
	if maxLines <= 0 {
		return ""
	}
	lines := strings.Split(output, "\n")
	var out []string
	for _, name := range names {
		if pkg, isPkg := strings.CutPrefix(name, goPkgKey); isPkg {
			out = append(out, goPkgBlock(lines, pkg)...)
		} else {
			out = append(out, goTestBlock(lines, name)...)
		}
	}
	if len(out) > maxLines {
		cut := len(out) - (maxLines - 1)
		out = append(out[:maxLines-1], fmt.Sprintf("... (%d lines cut)", cut))
	}
	return strings.Join(out, "\n")
}

// goTestBlock returns each "--- FAIL: name" line with the indented lines that follow it.
func goTestBlock(lines []string, name string) []string {
	var block []string
	in := false
	for _, line := range lines {
		if m := goFailedTest.FindStringSubmatch(line); m != nil {
			in = m[1] == name
			if in {
				block = append(block, line)
			}
			continue
		}
		trimmed := strings.TrimLeft(line, " \t")
		indented := len(trimmed) < len(line)
		in = in && indented && !strings.HasPrefix(trimmed, "--- ") && !strings.HasPrefix(trimmed, "=== ")
		if in {
			block = append(block, line)
		}
	}
	return block
}

// goPkgBlock returns the package's FAIL line and the lines leading up to it, never reaching
// into the previous package's output. A panic is shown from its "panic:" line, so that
// a long stack trace does not push the message out. A package whose build failed gets its
// compiler output instead, wherever it appears earlier in the output.
func goPkgBlock(lines []string, pkg string) []string {
	start := 0
	for i, line := range lines {
		m := goFailedPkg.FindStringSubmatch(line)
		if m != nil && m[1] == pkg {
			if strings.Contains(line, "[build failed]") || strings.Contains(line, "[setup failed]") {
				return append(goBuildBlocks(lines[:i], pkg), line)
			}
			from := max(start, i-goPkgContext)
			for j := start; j < i; j++ {
				if strings.HasPrefix(lines[j], "panic:") {
					from = j
					break
				}
			}
			return lines[from : i+1]
		}
		if m != nil || goPassedPkg.MatchString(line) {
			start = i + 1
		}
	}
	return nil
}

// goBuildBlocks returns the "# <pkg>" compiler output blocks for pkg found in lines. Go
// prints these when a build fails, not next to the package's FAIL line. When pkg has no
// block of its own (a dependency failed to build), it returns the blocks of packages that
// have no FAIL line, or failing that, every block.
func goBuildBlocks(lines []string, pkg string) []string {
	type block struct {
		pkg   string
		lines []string
	}
	var blocks []block
	failed := map[string]bool{}
	in := false
	for _, line := range lines {
		if m := goFailedPkg.FindStringSubmatch(line); m != nil {
			failed[m[1]] = true
		}
		if rest, ok := strings.CutPrefix(line, "# "); ok {
			name, _, _ := strings.Cut(rest, " ")
			blocks = append(blocks, block{pkg: name, lines: []string{line}})
			in = true
			continue
		}
		in = in && !goEndsBuildBlock(line)
		if in {
			b := &blocks[len(blocks)-1]
			b.lines = append(b.lines, line)
		}
	}
	collect := func(keep func(block) bool) []string {
		var out []string
		for _, b := range blocks {
			if keep(b) {
				out = append(out, b.lines...)
			}
		}
		return out
	}
	if out := collect(func(b block) bool { return b.pkg == pkg }); out != nil {
		return out
	}
	if out := collect(func(b block) bool { return !failed[b.pkg] }); out != nil {
		return out
	}
	return collect(func(block) bool { return true })
}

// goEndsBuildBlock reports whether line is go test output that cannot be compiler output.
func goEndsBuildBlock(line string) bool {
	return line == "" || line == "FAIL" || line == "PASS" ||
		goFailedPkg.MatchString(line) || goPassedPkg.MatchString(line) ||
		strings.HasPrefix(line, "?") || strings.HasPrefix(line, "=== ") ||
		strings.HasPrefix(strings.TrimLeft(line, " \t"), "--- ")
}
