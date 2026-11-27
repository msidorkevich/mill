package hooks_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/msidorkevich/mill/internal/testenv"
)

const ghScript = "github/pr.sh"

// ghRequest is one request the fake GitHub API received.
type ghRequest struct {
	Method string
	Path   string
	Head   string // the head query parameter, for pull request lookups
	Auth   string
	Body   map[string]any
}

// ghServer is a fake GitHub REST API for one repository.
type ghServer struct {
	t     *testing.T
	owner string
	repo  string
	srv   *httptest.Server

	mu       sync.Mutex
	open     map[string]int // review branch -> open PR number
	noRepo   bool           // GET /repos/{o}/{r} answers 404
	requests []ghRequest

	statuses      []map[string]any // combined status entries of every commit
	checkRuns     []map[string]any // check runs of every commit
	probeStatus   int              // answer to the permission probe POST (default 422)
	defaultStatus int              // when set, GET commits/main/status answers this code
}

func ghNewServer(t *testing.T, owner, repo string) *ghServer {
	t.Helper()
	s := &ghServer{t: t, owner: owner, repo: repo, open: map[string]int{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *ghServer) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req := ghRequest{Method: r.Method, Path: r.URL.Path, Head: r.URL.Query().Get("head"), Auth: r.Header.Get("Authorization")}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Errorf("read request body: %v", err)
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &req.Body); err != nil {
			s.t.Errorf("request body is not JSON: %v: %s", err, data)
		}
	}
	s.requests = append(s.requests, req)

	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(v); err != nil {
			s.t.Errorf("write response: %v", err)
		}
	}
	if req.Auth != "Bearer test-token" {
		reply(http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return
	}
	repoPath := "/repos/" + s.owner + "/" + s.repo
	pullsPath := repoPath + "/pulls"
	html := func(n int) string { return fmt.Sprintf("https://github.example/%s/%s/pull/%d", s.owner, s.repo, n) }
	switch {
	case r.Method == http.MethodGet && r.URL.Path == repoPath:
		if s.noRepo {
			reply(http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		reply(http.StatusOK, map[string]string{"full_name": s.owner + "/" + s.repo, "default_branch": "main"})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, repoPath+"/commits/") && strings.HasSuffix(r.URL.Path, "/status"):
		if s.defaultStatus != 0 && r.URL.Path == repoPath+"/commits/main/status" {
			reply(s.defaultStatus, map[string]string{"message": "Resource not accessible by personal access token"})
			return
		}
		list := s.statuses
		if list == nil {
			list = []map[string]any{}
		}
		reply(http.StatusOK, map[string]any{"state": "pending", "statuses": list})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, repoPath+"/commits/") && strings.HasSuffix(r.URL.Path, "/check-runs"):
		list := s.checkRuns
		if list == nil {
			list = []map[string]any{}
		}
		reply(http.StatusOK, map[string]any{"total_count": len(list), "check_runs": list})
	case r.Method == http.MethodGet && r.URL.Path == pullsPath:
		if r.URL.Query().Get("state") != "open" {
			reply(http.StatusUnprocessableEntity, map[string]string{"message": "state=open expected"})
			return
		}
		branch, ok := strings.CutPrefix(req.Head, s.owner+":")
		var list []map[string]any
		if n, found := s.open[branch]; ok && found {
			list = append(list, map[string]any{"number": n, "html_url": html(n)})
		}
		if list == nil {
			list = []map[string]any{}
		}
		reply(http.StatusOK, list)
	case r.Method == http.MethodPost && r.URL.Path == pullsPath:
		head, _ := req.Body["head"].(string)
		if strings.HasPrefix(head, "mill-permission-probe-") {
			code := s.probeStatus
			if code == 0 {
				code = http.StatusUnprocessableEntity
			}
			reply(code, map[string]string{"message": "probe answer"})
			return
		}
		n := 100 + len(s.open)
		s.open[head] = n
		reply(http.StatusCreated, map[string]any{"number": n, "html_url": html(n)})
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, pullsPath+"/"):
		var n int
		if _, err := fmt.Sscanf(strings.TrimPrefix(r.URL.Path, pullsPath+"/"), "%d", &n); err != nil {
			reply(http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		reply(http.StatusOK, map[string]any{"number": n, "html_url": html(n)})
	default:
		reply(http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
}

func (s *ghServer) recorded() []ghRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ghRequest(nil), s.requests...)
}

// ghOnly returns the recorded requests with the given method.
func (s *ghServer) ghOnly(method string) []ghRequest {
	var out []ghRequest
	for _, r := range s.recorded() {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

// ghRequireTools skips the test when the script's tools are not installed.
func ghRequireTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range append([]string{"sh", "curl", "jq"}, tools...) {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found on PATH: the GitHub hook tests need sh, curl and jq", tool)
		}
	}
}

// ghResult is the outcome of one hook run.
type ghResult struct {
	Stdout, Stderr string
	Code           int
}

// ghRunHook runs the script with stdin, a minimal environment plus env, in dir.
func ghRunHook(t *testing.T, dir, stdin string, env ...string) ghResult {
	t.Helper()
	script, err := filepath.Abs(ghScript)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", script)
	cmd.Dir = dir
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
		"GIT_CONFIG_NOSYSTEM=1",
	}, env...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	res := ghResult{Code: 0}
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run %s: %v", script, err)
		}
		res.Code = exitErr.ExitCode()
	}
	res.Stdout, res.Stderr = stdout.String(), stderr.String()
	return res
}

// ghInput builds a mill/hook@1 document; extra fields override the defaults.
func ghInput(t *testing.T, extra map[string]any) string {
	t.Helper()
	doc := map[string]any{
		"schema":        "mill/hook@1",
		"event":         "publish",
		"goal_id":       "g1",
		"task_id":       "g1/feature",
		"title":         "Add the feature",
		"description":   "Implements the feature.",
		"review_branch": "mill/g1/feature",
		"base_branch":   "main",
		"head_sha":      "0123456789abcdef0123456789abcdef01234567",
		"remote":        "origin",
		"remote_url":    "git@github.com:acme/widgets.git",
	}
	for k, v := range extra {
		doc[k] = v
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func ghEnv(s *ghServer) []string {
	return []string{"MILL_GITHUB_API=" + s.srv.URL, "GITHUB_TOKEN=test-token"}
}

func ghExpectSuccess(t *testing.T, res ghResult, wantURL string) {
	t.Helper()
	if res.Code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", res.Code, res.Stdout, res.Stderr)
	}
	if got := strings.TrimSpace(res.Stdout); got != wantURL {
		t.Fatalf("stdout = %q, want the PR url %q", got, wantURL)
	}
}

func TestGitHubScriptIsExecutable(t *testing.T) {
	info, err := os.Stat(ghScript)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("%s mode %v: want executable", ghScript, info.Mode())
	}
}

func TestGitHubScriptUsesOnlyGitCurlAndJq(t *testing.T) {
	data, err := os.ReadFile(ghScript)
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	if !strings.HasPrefix(src, "#!/bin/sh\n") {
		t.Fatalf("%s must start with #!/bin/sh", ghScript)
	}
	for _, forbidden := range []string{"gh ", "python", "perl", "node ", "wget", "ruby"} {
		for _, line := range strings.Split(src, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if strings.Contains(line, forbidden) {
				t.Errorf("%s uses %q: only sh, git, curl and jq are allowed: %s", ghScript, forbidden, line)
			}
		}
	}
}

func TestGitHubCreatesDraftPRWhenNoneIsOpen(t *testing.T) {
	ghRequireTools(t)
	s := ghNewServer(t, "acme", "widgets")
	res := ghRunHook(t, t.TempDir(), ghInput(t, nil), ghEnv(s)...)
	ghExpectSuccess(t, res, "https://github.example/acme/widgets/pull/100")

	gets := s.ghOnly(http.MethodGet)
	if len(gets) != 1 || gets[0].Head != "acme:mill/g1/feature" {
		t.Fatalf("lookups = %+v, want one GET with head acme:mill/g1/feature", gets)
	}
	posts := s.ghOnly(http.MethodPost)
	if len(posts) != 1 {
		t.Fatalf("POSTs = %+v, want exactly one", posts)
	}
	b := posts[0].Body
	if b["draft"] != true || b["head"] != "mill/g1/feature" || b["base"] != "main" ||
		b["title"] != "Add the feature" || b["body"] != "Implements the feature." {
		t.Fatalf("create body = %v, want draft PR from mill/g1/feature into main", b)
	}
	if p := s.ghOnly(http.MethodPatch); len(p) != 0 {
		t.Fatalf("PATCHes = %+v, want none on create", p)
	}
}

func TestGitHubUpdatesExistingPRWithoutDuplicate(t *testing.T) {
	ghRequireTools(t)
	s := ghNewServer(t, "acme", "widgets")
	s.open["mill/g1/feature"] = 7
	res := ghRunHook(t, t.TempDir(), ghInput(t, map[string]any{"event": "update", "title": "New title"}), ghEnv(s)...)
	ghExpectSuccess(t, res, "https://github.example/acme/widgets/pull/7")

	if posts := s.ghOnly(http.MethodPost); len(posts) != 0 {
		t.Fatalf("POSTs = %+v, want none when a PR is open", posts)
	}
	patches := s.ghOnly(http.MethodPatch)
	if len(patches) != 1 || patches[0].Path != "/repos/acme/widgets/pulls/7" {
		t.Fatalf("PATCHes = %+v, want one to /repos/acme/widgets/pulls/7", patches)
	}
	if b := patches[0].Body; b["title"] != "New title" || b["base"] != "main" || b["body"] != "Implements the feature." {
		t.Fatalf("update body = %v", b)
	}
}

func TestGitHubSecondRunUpdatesWhatFirstCreated(t *testing.T) {
	ghRequireTools(t)
	s := ghNewServer(t, "acme", "widgets")
	dir := t.TempDir()
	ghExpectSuccess(t, ghRunHook(t, dir, ghInput(t, nil), ghEnv(s)...), "https://github.example/acme/widgets/pull/100")
	ghExpectSuccess(t, ghRunHook(t, dir, ghInput(t, nil), ghEnv(s)...), "https://github.example/acme/widgets/pull/100")
	if posts, patches := s.ghOnly(http.MethodPost), s.ghOnly(http.MethodPatch); len(posts) != 1 || len(patches) != 1 {
		t.Fatalf("POSTs = %d, PATCHes = %d; want 1 and 1", len(posts), len(patches))
	}
}

func TestGitHubUpdateRetargetsToNewBase(t *testing.T) {
	ghRequireTools(t)
	s := ghNewServer(t, "acme", "widgets")
	s.open["mill/g1/child"] = 12
	in := ghInput(t, map[string]any{
		"event":         "update",
		"review_branch": "mill/g1/child",
		"base_branch":   "main",
		"previous_base": "mill/g1/parent",
	})
	ghExpectSuccess(t, ghRunHook(t, t.TempDir(), in, ghEnv(s)...), "https://github.example/acme/widgets/pull/12")
	patches := s.ghOnly(http.MethodPatch)
	if len(patches) != 1 || patches[0].Body["base"] != "main" {
		t.Fatalf("PATCHes = %+v, want one setting base main", patches)
	}
}

func TestGitHubStackedBodyNamesParentPR(t *testing.T) {
	ghRequireTools(t)
	s := ghNewServer(t, "acme", "widgets")
	s.open["mill/g1/parent"] = 41
	in := ghInput(t, map[string]any{
		"review_branch":        "mill/g1/child",
		"base_branch":          "mill/g1/parent",
		"parent_task_id":       "g1/parent",
		"parent_review_branch": "mill/g1/parent",
	})
	ghExpectSuccess(t, ghRunHook(t, t.TempDir(), in, ghEnv(s)...), "https://github.example/acme/widgets/pull/101")
	posts := s.ghOnly(http.MethodPost)
	if len(posts) != 1 {
		t.Fatalf("POSTs = %+v, want one", posts)
	}
	want := "Stacked on #41: merge that first\n\nImplements the feature."
	if b := posts[0].Body; b["body"] != want || b["base"] != "mill/g1/parent" {
		t.Fatalf("create body = %v, want body %q and base mill/g1/parent", b, want)
	}
}

func TestGitHubStackedBodyNamesParentBranchWithoutPR(t *testing.T) {
	ghRequireTools(t)
	s := ghNewServer(t, "acme", "widgets")
	in := ghInput(t, map[string]any{
		"review_branch":        "mill/g1/child",
		"base_branch":          "mill/g1/parent",
		"parent_review_branch": "mill/g1/parent",
	})
	ghExpectSuccess(t, ghRunHook(t, t.TempDir(), in, ghEnv(s)...), "https://github.example/acme/widgets/pull/100")
	want := "Stacked on mill/g1/parent\n\nImplements the feature."
	if posts := s.ghOnly(http.MethodPost); len(posts) != 1 || posts[0].Body["body"] != want {
		t.Fatalf("POSTs = %+v, want one with body %q", posts, want)
	}
}

func TestGitHubResolvesOwnerRepoFromRemoteURL(t *testing.T) {
	ghRequireTools(t)
	for _, url := range []string{
		"git@github.com:acme/widgets.git",
		"git@github.com:acme/widgets",
		"https://github.com/acme/widgets",
		"https://github.com/acme/widgets.git",
	} {
		t.Run(url, func(t *testing.T) {
			s := ghNewServer(t, "acme", "widgets")
			res := ghRunHook(t, t.TempDir(), ghInput(t, map[string]any{"remote_url": url}), ghEnv(s)...)
			ghExpectSuccess(t, res, "https://github.example/acme/widgets/pull/100")
			for _, r := range s.recorded() {
				if !strings.HasPrefix(r.Path, "/repos/acme/widgets/") {
					t.Fatalf("request to %s, want /repos/acme/widgets/...", r.Path)
				}
			}
		})
	}
}

func TestGitHubMissingTokenFailsWithWhatToSet(t *testing.T) {
	ghRequireTools(t)
	s := ghNewServer(t, "acme", "widgets")
	res := ghRunHook(t, t.TempDir(), ghInput(t, nil), "MILL_GITHUB_API="+s.srv.URL)
	if res.Code != 1 {
		t.Fatalf("exit %d, want 1\nstderr: %s", res.Code, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "set GITHUB_TOKEN and list it in hook_env") {
		t.Fatalf("stderr = %q, want what to set", res.Stderr)
	}
	if n := len(s.recorded()); n != 0 {
		t.Fatalf("%d requests without a token, want none", n)
	}
}

func TestGitHubHTTPErrorReportsStatusAndMessage(t *testing.T) {
	ghRequireTools(t)
	s := ghNewServer(t, "acme", "widgets")
	res := ghRunHook(t, t.TempDir(), ghInput(t, nil), "MILL_GITHUB_API="+s.srv.URL, "GITHUB_TOKEN=wrong")
	if res.Code == 0 {
		t.Fatalf("exit 0 on HTTP 401, want non-zero; stdout: %s", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "401") || !strings.Contains(res.Stderr, "Bad credentials") {
		t.Fatalf("stderr = %q, want the status and the message", res.Stderr)
	}
	if posts := s.ghOnly(http.MethodPost); len(posts) != 0 {
		t.Fatalf("POSTs = %+v after a failed lookup, want none", posts)
	}
}

// ghGitRepo makes a temporary git repository whose origin is url.
func ghGitRepo(t *testing.T, url string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", url}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func TestGitHubCheckModeSucceedsWithRepositoryAccess(t *testing.T) {
	ghRequireTools(t, "git")
	s := ghNewServer(t, "acme", "widgets")
	dir := ghGitRepo(t, "https://github.com/acme/widgets.git")
	res := ghRunHook(t, dir, "", append(ghEnv(s), "MILL_HOOK_CHECK=1")...)
	if res.Code != 0 {
		t.Fatalf("exit %d, want 0\nstderr: %s", res.Code, res.Stderr)
	}
	reqs := s.recorded()
	if len(reqs) != 2 || reqs[0].Method != http.MethodGet || reqs[0].Path != "/repos/acme/widgets" ||
		reqs[1].Method != http.MethodPost {
		t.Fatalf("requests = %+v, want GET /repos/acme/widgets and the permission probe", reqs)
	}
}

func TestGitHubCheckModeFailsWithoutRepositoryAccess(t *testing.T) {
	ghRequireTools(t, "git")
	s := ghNewServer(t, "acme", "widgets")
	s.noRepo = true
	dir := ghGitRepo(t, "git@github.com:acme/widgets.git")
	res := ghRunHook(t, dir, "", append(ghEnv(s), "MILL_HOOK_CHECK=1")...)
	if res.Code == 0 {
		t.Fatalf("exit 0 without repository access, want non-zero")
	}
	if !strings.Contains(res.Stderr, "404") || !strings.Contains(res.Stderr, "Not Found") {
		t.Fatalf("stderr = %q, want the status and the message", res.Stderr)
	}
	if n := len(s.ghOnly(http.MethodPost)) + len(s.ghOnly(http.MethodPatch)); n != 0 {
		t.Fatalf("check mode changed %d PRs, want none", n)
	}
}

func TestGitHubCheckModeFailsWithoutToken(t *testing.T) {
	ghRequireTools(t, "git")
	s := ghNewServer(t, "acme", "widgets")
	dir := ghGitRepo(t, "git@github.com:acme/widgets.git")
	res := ghRunHook(t, dir, "", "MILL_GITHUB_API="+s.srv.URL, "MILL_HOOK_CHECK=1")
	if res.Code != 1 || !strings.Contains(res.Stderr, "set GITHUB_TOKEN and list it in hook_env") {
		t.Fatalf("exit %d, stderr %q; want 1 and what to set", res.Code, res.Stderr)
	}
}

func TestGitHubCheckModeFailsWithoutTools(t *testing.T) {
	ghRequireTools(t)
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not found")
	}
	// A PATH holding only sh: curl and jq are missing.
	bin := t.TempDir()
	if err := os.Symlink(sh, filepath.Join(bin, "sh")); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(ghScript)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, script)
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"PATH=" + bin, "GITHUB_TOKEN=test-token", "MILL_HOOK_CHECK=1", "TMPDIR=" + t.TempDir()}
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("check passed without curl and jq: %s", out)
	}
	if !strings.Contains(string(out), "curl not found") {
		t.Fatalf("output = %q, want the missing tool named", out)
	}
}

func TestGitHubScriptPassesShellcheck(t *testing.T) {
	if _, err := exec.LookPath("shellcheck"); err != nil {
		t.Skip("shellcheck not found on PATH")
	}
	out, err := exec.Command("shellcheck", "-s", "sh", ghScript).CombinedOutput()
	if err != nil {
		t.Fatalf("shellcheck -s sh %s: %v\n%s", ghScript, err, out)
	}
}

func TestGitHubHookKeepsTokenOffCurlArgs(t *testing.T) {
	ghRequireTools(t)
	realCurl, err := exec.LookPath("curl")
	if err != nil {
		t.Fatal(err)
	}
	// A fake curl first on PATH: it records each argument on its own line, then runs the real one.
	bin := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "curl-args")
	fake := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >>'" + argsFile + "'; done\nexec '" + realCurl + "' \"$@\"\n"
	testenv.WriteExecutable(t, filepath.Join(bin, "curl"), []byte(fake))
	s := ghNewServer(t, "acme", "widgets")
	env := append(ghEnv(s), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	res := ghRunHook(t, t.TempDir(), ghInput(t, nil), env...)
	ghExpectSuccess(t, res, "https://github.example/acme/widgets/pull/100")

	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("the fake curl recorded nothing: %v", err)
	}
	if !strings.Contains(string(data), s.srv.URL) {
		t.Fatalf("recorded curl args do not name the server %s: the fake curl was not used\n%s", s.srv.URL, data)
	}
	for _, arg := range strings.Split(string(data), "\n") {
		if strings.Contains(arg, "test-token") {
			t.Errorf("curl argument %q contains the token", arg)
		}
	}
	reqs := s.recorded()
	if len(reqs) != 2 {
		t.Fatalf("requests = %+v, want a lookup and a create", reqs)
	}
	for _, r := range reqs {
		if r.Auth != "Bearer test-token" {
			t.Errorf("%s %s: Authorization = %q, want %q", r.Method, r.Path, r.Auth, "Bearer test-token")
		}
	}
}

// ghStatusDoc is the mill/status@1 object the status event prints.
type ghStatusDoc struct {
	Schema string `json:"schema"`
	State  string `json:"state"`
	URL    string `json:"url"`
	Checks []struct {
		Name    string `json:"name"`
		URL     string `json:"url"`
		State   string `json:"state"`
		Summary string `json:"summary"`
	} `json:"checks"`
}

// ghRunStatus runs the status event and decodes its stdout, which must hold exactly one object.
func ghRunStatus(t *testing.T, s *ghServer, env ...string) ghStatusDoc {
	t.Helper()
	res := ghRunHook(t, t.TempDir(), ghInput(t, map[string]any{"event": "status"}), append(ghEnv(s), env...)...)
	if res.Code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", res.Code, res.Stdout, res.Stderr)
	}
	dec := json.NewDecoder(strings.NewReader(res.Stdout))
	var doc ghStatusDoc
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, res.Stdout)
	}
	if dec.More() {
		t.Fatalf("stdout holds more than one JSON value:\n%s", res.Stdout)
	}
	if doc.Schema != "mill/status@1" {
		t.Fatalf("schema = %q, want mill/status@1", doc.Schema)
	}
	return doc
}

const ghSHA = "0123456789abcdef0123456789abcdef01234567"

func TestGhStatusFailedCheckRunReportsFailureWithSummaryAndPR(t *testing.T) {
	ghRequireTools(t)
	s := ghNewServer(t, "acme", "widgets")
	s.open["mill/g1/feature"] = 9
	long := strings.Repeat("é", 1500) // 3000 bytes
	s.checkRuns = []map[string]any{
		{"name": "build", "status": "completed", "conclusion": "success", "html_url": "https://github.example/runs/1"},
		{"name": "test", "status": "completed", "conclusion": "failure", "html_url": "https://github.example/runs/2",
			"output": map[string]any{"title": "2 tests failed", "summary": long}},
	}
	doc := ghRunStatus(t, s)
	if doc.State != "failure" {
		t.Fatalf("state = %q, want failure", doc.State)
	}
	if doc.URL != "https://github.example/acme/widgets/pull/9" {
		t.Fatalf("url = %q, want the open PR's html_url", doc.URL)
	}
	if len(doc.Checks) != 2 {
		t.Fatalf("checks = %+v, want both runs", doc.Checks)
	}
	c := doc.Checks[1]
	if c.Name != "test" || c.URL != "https://github.example/runs/2" || c.State != "failure" {
		t.Fatalf("failed check = %+v, want test with its html_url and state failure", c)
	}
	if !strings.HasPrefix(c.Summary, "2 tests failed\né") {
		t.Fatalf("summary = %q, want the output title, a newline, then the summary", c.Summary[:min(40, len(c.Summary))])
	}
	if n := len(c.Summary); n > 2048 || n < 2046 {
		t.Fatalf("summary is %d bytes, want cut to 2048", n)
	}
	if !utf8.ValidString(c.Summary) {
		t.Fatalf("summary was cut inside a character")
	}
	if doc.Checks[0].State != "success" || doc.Checks[0].Summary != "" {
		t.Fatalf("passing check = %+v, want success without a summary", doc.Checks[0])
	}
	var commitPaths []string
	for _, r := range s.ghOnly(http.MethodGet) {
		if strings.Contains(r.Path, "/commits/") {
			commitPaths = append(commitPaths, r.Path)
		}
	}
	want := []string{"/repos/acme/widgets/commits/" + ghSHA + "/status", "/repos/acme/widgets/commits/" + ghSHA + "/check-runs"}
	if strings.Join(commitPaths, " ") != strings.Join(want, " ") {
		t.Fatalf("commit requests = %v, want %v", commitPaths, want)
	}
}

func TestGhStatusStates(t *testing.T) {
	ghRequireTools(t)
	for _, tc := range []struct {
		name     string
		statuses []map[string]any
		runs     []map[string]any
		want     string
	}{
		{"mixed pending and success is pending",
			[]map[string]any{{"context": "ci/lint", "state": "success", "target_url": "https://ci.example/1"}},
			[]map[string]any{{"name": "test", "status": "in_progress", "conclusion": nil}},
			"pending"},
		{"pending status is pending",
			[]map[string]any{{"context": "ci/lint", "state": "pending"}},
			[]map[string]any{{"name": "test", "status": "completed", "conclusion": "success"}},
			"pending"},
		{"all success is success",
			[]map[string]any{{"context": "ci/lint", "state": "success"}},
			[]map[string]any{{"name": "test", "status": "completed", "conclusion": "success"}},
			"success"},
		{"nothing is none", nil, nil, "none"},
		{"status error is failure",
			[]map[string]any{{"context": "ci/deploy", "state": "error", "target_url": "https://ci.example/2"}},
			[]map[string]any{{"name": "test", "status": "completed", "conclusion": "success"}},
			"failure"},
		{"timed out run is failure", nil,
			[]map[string]any{{"name": "test", "status": "completed", "conclusion": "timed_out"}},
			"failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := ghNewServer(t, "acme", "widgets")
			s.statuses, s.checkRuns = tc.statuses, tc.runs
			doc := ghRunStatus(t, s)
			if doc.State != tc.want {
				t.Fatalf("state = %q, want %q (checks %+v)", doc.State, tc.want, doc.Checks)
			}
			if got, want := len(doc.Checks), len(tc.statuses)+len(tc.runs); got != want {
				t.Fatalf("checks = %+v, want %d entries", doc.Checks, want)
			}
			if doc.URL != "" {
				t.Fatalf("url = %q without an open PR, want none", doc.URL)
			}
		})
	}
}

func TestGhStatusListsStatusContextAndTargetURL(t *testing.T) {
	ghRequireTools(t)
	s := ghNewServer(t, "acme", "widgets")
	s.statuses = []map[string]any{{"context": "ci/deploy", "state": "error", "target_url": "https://ci.example/2"}}
	doc := ghRunStatus(t, s)
	if len(doc.Checks) != 1 || doc.Checks[0].Name != "ci/deploy" || doc.Checks[0].URL != "https://ci.example/2" ||
		doc.Checks[0].State != "failure" {
		t.Fatalf("checks = %+v, want ci/deploy with its target_url, state failure", doc.Checks)
	}
}

func TestGhStatusEventFromEnvironment(t *testing.T) {
	ghRequireTools(t)
	s := ghNewServer(t, "acme", "widgets")
	res := ghRunHook(t, t.TempDir(), ghInput(t, nil), append(ghEnv(s), "MILL_HOOK_EVENT=status")...)
	if res.Code != 0 || !strings.Contains(res.Stdout, `"mill/status@1"`) {
		t.Fatalf("exit %d, stdout %q; want a mill/status@1 object\nstderr: %s", res.Code, res.Stdout, res.Stderr)
	}
	if posts := s.ghOnly(http.MethodPost); len(posts) != 0 {
		t.Fatalf("POSTs = %+v on the status event, want none", posts)
	}
}

func ghRunCheck(t *testing.T, s *ghServer, env ...string) ghResult {
	t.Helper()
	ghRequireTools(t, "git")
	dir := ghGitRepo(t, "git@github.com:acme/widgets.git")
	return ghRunHook(t, dir, "", append(append(ghEnv(s), "MILL_HOOK_CHECK=1"), env...)...)
}

func TestGhCheckPassesWhenProbeGets422(t *testing.T) {
	s := ghNewServer(t, "acme", "widgets")
	res := ghRunCheck(t, s)
	if res.Code != 0 {
		t.Fatalf("exit %d, want 0\nstderr: %s", res.Code, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "token may create pull requests") {
		t.Fatalf("stdout = %q, want the write check reported", res.Stdout)
	}
	posts := s.ghOnly(http.MethodPost)
	if len(posts) != 1 || posts[0].Path != "/repos/acme/widgets/pulls" {
		t.Fatalf("POSTs = %+v, want one probe to /repos/acme/widgets/pulls", posts)
	}
	head, _ := posts[0].Body["head"].(string)
	if !strings.HasPrefix(head, "mill-permission-probe-") || len(head) <= len("mill-permission-probe-") || posts[0].Body["base"] != "main" {
		t.Fatalf("probe body = %v, want head mill-permission-probe-<random> and base main", posts[0].Body)
	}
	if len(s.open) != 0 {
		t.Fatalf("open PRs = %v after the probe, want none", s.open)
	}
}

func TestGhCheckFailsWhenTokenCannotCreatePulls(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusNotFound} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			s := ghNewServer(t, "acme", "widgets")
			s.probeStatus = code
			res := ghRunCheck(t, s)
			if res.Code == 0 {
				t.Fatalf("exit 0 with HTTP %d from the probe, want non-zero", code)
			}
			want := "the token cannot create pull requests: give it Pull requests: Read and write on acme/widgets"
			if !strings.Contains(res.Stderr, want) {
				t.Fatalf("stderr = %q, want %q", res.Stderr, want)
			}
		})
	}
}

func TestGhCheckFailsOnUnexpectedProbeStatus(t *testing.T) {
	s := ghNewServer(t, "acme", "widgets")
	s.probeStatus = http.StatusInternalServerError
	res := ghRunCheck(t, s)
	if res.Code == 0 || !strings.Contains(res.Stderr, "500") {
		t.Fatalf("exit %d, stderr %q; want a failure naming HTTP 500", res.Code, res.Stderr)
	}
}

func TestGhCheckStatusEventFailsWhenStatusesAreRefused(t *testing.T) {
	s := ghNewServer(t, "acme", "widgets")
	s.defaultStatus = http.StatusForbidden
	res := ghRunCheck(t, s, "MILL_HOOK_EVENT=status")
	if res.Code == 0 {
		t.Fatalf("exit 0 when commit statuses are refused, want non-zero")
	}
	for _, want := range []string{"403", "Commit statuses: Read", "Checks: Read", "acme/widgets"} {
		if !strings.Contains(res.Stderr, want) {
			t.Fatalf("stderr = %q, want it to contain %q", res.Stderr, want)
		}
	}
}

func TestGhCheckStatusEventPassesWhenStatusesAreReadable(t *testing.T) {
	s := ghNewServer(t, "acme", "widgets")
	res := ghRunCheck(t, s, "MILL_HOOK_EVENT=status")
	if res.Code != 0 {
		t.Fatalf("exit %d, want 0\nstderr: %s", res.Code, res.Stderr)
	}
	var read bool
	for _, r := range s.ghOnly(http.MethodGet) {
		read = read || r.Path == "/repos/acme/widgets/commits/main/status"
	}
	if !read {
		t.Fatalf("requests = %+v, want GET commits/main/status", s.recorded())
	}
}

func TestGhDraftDefaultAndDisabled(t *testing.T) {
	ghRequireTools(t)
	for _, tc := range []struct {
		env  []string
		want bool
	}{
		{nil, true},
		{[]string{"MILL_GITHUB_DRAFT=1"}, true},
		{[]string{"MILL_GITHUB_DRAFT=0"}, false},
	} {
		t.Run(fmt.Sprint(tc.env), func(t *testing.T) {
			s := ghNewServer(t, "acme", "widgets")
			res := ghRunHook(t, t.TempDir(), ghInput(t, nil), append(ghEnv(s), tc.env...)...)
			ghExpectSuccess(t, res, "https://github.example/acme/widgets/pull/100")
			posts := s.ghOnly(http.MethodPost)
			if len(posts) != 1 || posts[0].Body["draft"] != tc.want {
				t.Fatalf("POSTs = %+v, want one with draft %v", posts, tc.want)
			}
		})
	}
}
