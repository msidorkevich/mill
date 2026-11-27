package hooks_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/msidorkevich/mill/internal/testenv"
)

const bbScript = "bitbucket/pr.sh"

// bbRequest is one request the fake Bitbucket API received.
type bbRequest struct {
	Method   string
	Path     string
	RawQuery string
	Q        string // the decoded q query parameter, for pull request lookups
	Auth     string
	Body     map[string]any
}

// bbServer is a fake Bitbucket Cloud REST 2.0 API for one repository.
type bbServer struct {
	t         *testing.T
	workspace string
	repo      string
	srv       *httptest.Server
	wantAuth  string

	// mu guards every field below. The handler holds it for each request; tests use
	// bbSet, bbOpenPRs and recorded.
	mu       sync.Mutex
	open     map[string]int // review branch -> open PR id
	noRepo   bool           // GET /repositories/{ws}/{repo} answers 404
	requests []bbRequest
	// statuses holds the build statuses of each commit or branch, served pageSize at a time.
	statuses map[string][]map[string]any
	pageSize int
	// probeStatus answers the empty POST pullrequests permission probe (default 400).
	probeStatus int
	// statusesStatus, when set, answers every GET of commit statuses with this error status.
	statusesStatus int
}

func bbNewServer(t *testing.T, workspace, repo string) *bbServer {
	t.Helper()
	s := &bbServer{t: t, workspace: workspace, repo: repo, open: map[string]int{}, statuses: map[string][]map[string]any{}, wantAuth: "Bearer test-token"}
	// srv is set before Start, so the handler never sees it change.
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(s.handle))
	s.srv.Start()
	t.Cleanup(s.srv.Close)
	return s
}

var bbQueryPattern = regexp.MustCompile(`^source\.branch\.name="([^"]+)" AND state="OPEN"$`)

func (s *bbServer) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req := bbRequest{
		Method:   r.Method,
		Path:     r.URL.Path,
		RawQuery: r.URL.RawQuery,
		Q:        r.URL.Query().Get("q"),
		Auth:     r.Header.Get("Authorization"),
	}
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
	fail := func(status int, msg string) {
		reply(status, map[string]any{"type": "error", "error": map[string]string{"message": msg}})
	}
	if req.Auth != s.wantAuth {
		fail(http.StatusUnauthorized, "Token is invalid or not supported for this endpoint.")
		return
	}
	repoPath := "/repositories/" + s.workspace + "/" + s.repo
	pullsPath := repoPath + "/pullrequests"
	pr := func(id int) map[string]any {
		return map[string]any{
			"id":    id,
			"links": map[string]any{"html": map[string]string{"href": s.html(id)}},
		}
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == repoPath:
		if s.noRepo {
			fail(http.StatusNotFound, "Repository not found")
			return
		}
		reply(http.StatusOK, map[string]any{
			"full_name":  s.workspace + "/" + s.repo,
			"mainbranch": map[string]string{"name": "main"},
		})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, repoPath+"/commit/") &&
		strings.HasSuffix(r.URL.Path, "/statuses"):
		if s.statusesStatus != 0 {
			fail(s.statusesStatus, "Access denied")
			return
		}
		ref := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, repoPath+"/commit/"), "/statuses")
		all := s.statuses[ref]
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			if _, err := fmt.Sscanf(p, "%d", &page); err != nil {
				fail(http.StatusBadRequest, "bad page "+p)
				return
			}
		}
		size := s.pageSize
		if size == 0 {
			size = len(all) + 1
		}
		lo, hi := min((page-1)*size, len(all)), min(page*size, len(all))
		body := map[string]any{"values": append([]map[string]any{}, all[lo:hi]...), "page": page, "pagelen": size}
		if hi < len(all) {
			body["next"] = fmt.Sprintf("%s%s?page=%d", s.srv.URL, r.URL.Path, page+1)
		}
		reply(http.StatusOK, body)
	case r.Method == http.MethodGet && r.URL.Path == pullsPath:
		m := bbQueryPattern.FindStringSubmatch(req.Q)
		if m == nil {
			fail(http.StatusBadRequest, "unexpected q: "+req.Q)
			return
		}
		values := []map[string]any{}
		if id, ok := s.open[m[1]]; ok {
			values = append(values, pr(id))
		}
		reply(http.StatusOK, map[string]any{"values": values, "pagelen": 10, "page": 1})
	case r.Method == http.MethodPost && r.URL.Path == pullsPath:
		id := 100 + len(s.open)
		src, _ := req.Body["source"].(map[string]any)
		branch, _ := src["branch"].(map[string]any)
		name, _ := branch["name"].(string)
		if name == "" {
			status := s.probeStatus
			if status == 0 {
				status = http.StatusBadRequest
			}
			fail(status, "source: This field is required.")
			return
		}
		s.open[name] = id
		reply(http.StatusCreated, pr(id))
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, pullsPath+"/"):
		var id int
		if _, err := fmt.Sscanf(strings.TrimPrefix(r.URL.Path, pullsPath+"/"), "%d", &id); err != nil {
			fail(http.StatusNotFound, "Not Found")
			return
		}
		reply(http.StatusOK, pr(id))
	default:
		fail(http.StatusNotFound, "Not Found")
	}
}

func (s *bbServer) html(id int) string {
	return fmt.Sprintf("https://bitbucket.example/%s/%s/pull-requests/%d", s.workspace, s.repo, id)
}

// bbSet changes the server's scripted state under the lock the handler holds. Tests write
// that state only through it.
func (s *bbServer) bbSet(change func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	change()
}

// bbOpenPRs returns a copy of the open pull requests by review branch.
func (s *bbServer) bbOpenPRs() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.open)
}

func (s *bbServer) recorded() []bbRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bbRequest(nil), s.requests...)
}

// bbOnly returns the recorded requests with the given method.
func (s *bbServer) bbOnly(method string) []bbRequest {
	var out []bbRequest
	for _, r := range s.recorded() {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

// bbRequireTools skips the test when the script's tools are not installed.
func bbRequireTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range append([]string{"sh", "curl", "jq"}, tools...) {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found on PATH: the Bitbucket hook tests need sh, curl and jq", tool)
		}
	}
}

// bbResult is the outcome of one hook run.
type bbResult struct {
	Stdout, Stderr string
	Code           int
}

// bbRunHook runs the script with stdin, a minimal environment plus env, in dir.
func bbRunHook(t *testing.T, dir, stdin string, env ...string) bbResult {
	t.Helper()
	script, err := filepath.Abs(bbScript)
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
	res := bbResult{}
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

// bbInput builds a mill/hook@1 document; extra fields override the defaults.
func bbInput(t *testing.T, extra map[string]any) string {
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
		"remote_url":    "git@bitbucket.org:acme/widgets.git",
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

func bbEnv(s *bbServer) []string {
	return []string{"MILL_BITBUCKET_API=" + s.srv.URL, "BITBUCKET_TOKEN=test-token"}
}

func bbExpectSuccess(t *testing.T, res bbResult, wantURL string) {
	t.Helper()
	if res.Code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", res.Code, res.Stdout, res.Stderr)
	}
	if got := strings.TrimSpace(res.Stdout); got != wantURL {
		t.Fatalf("stdout = %q, want the PR url %q", got, wantURL)
	}
}

// bbDig follows keys through nested JSON objects.
func bbDig(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func TestBitbucketScriptIsExecutable(t *testing.T) {
	info, err := os.Stat(bbScript)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("%s mode %v: want executable", bbScript, info.Mode())
	}
}

func TestBitbucketScriptUsesOnlyGitCurlAndJq(t *testing.T) {
	data, err := os.ReadFile(bbScript)
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	if !strings.HasPrefix(src, "#!/bin/sh\n") {
		t.Fatalf("%s must start with #!/bin/sh", bbScript)
	}
	for _, forbidden := range []string{"bb ", "python", "perl", "node ", "wget", "ruby"} {
		for _, line := range strings.Split(src, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if strings.Contains(line, forbidden) {
				t.Errorf("%s uses %q: only sh, git, curl and jq are allowed: %s", bbScript, forbidden, line)
			}
		}
	}
}

func TestBitbucketFindQueryIsEncodedAndFiltersOpenPRsFromBranch(t *testing.T) {
	bbRequireTools(t)
	s := bbNewServer(t, "acme", "widgets")
	res := bbRunHook(t, t.TempDir(), bbInput(t, nil), bbEnv(s)...)
	bbExpectSuccess(t, res, s.html(100))

	gets := s.bbOnly(http.MethodGet)
	if len(gets) != 1 {
		t.Fatalf("lookups = %+v, want one GET", gets)
	}
	g := gets[0]
	if g.Path != "/repositories/acme/widgets/pullrequests" {
		t.Fatalf("lookup path = %q", g.Path)
	}
	if want := `source.branch.name="mill/g1/feature" AND state="OPEN"`; g.Q != want {
		t.Fatalf("q = %q, want %q", g.Q, want)
	}
	if strings.ContainsAny(g.RawQuery, "\" ") {
		t.Fatalf("raw query %q is not URL-encoded", g.RawQuery)
	}
	if !strings.Contains(g.RawQuery, "%22") {
		t.Fatalf("raw query %q: want the quotes encoded as %%22", g.RawQuery)
	}
}

func TestBitbucketCreatesDraftPRWhenNoneIsOpen(t *testing.T) {
	bbRequireTools(t)
	s := bbNewServer(t, "acme", "widgets")
	res := bbRunHook(t, t.TempDir(), bbInput(t, nil), bbEnv(s)...)
	bbExpectSuccess(t, res, s.html(100))

	posts := s.bbOnly(http.MethodPost)
	if len(posts) != 1 {
		t.Fatalf("POSTs = %+v, want one", posts)
	}
	p := posts[0]
	if p.Path != "/repositories/acme/widgets/pullrequests" {
		t.Fatalf("POST path = %q", p.Path)
	}
	checks := map[string]any{
		"title":       bbDig(p.Body, "title"),
		"description": bbDig(p.Body, "description"),
		"source":      bbDig(p.Body, "source", "branch", "name"),
		"destination": bbDig(p.Body, "destination", "branch", "name"),
		"draft":       bbDig(p.Body, "draft"),
	}
	want := map[string]any{
		"title":       "Add the feature",
		"description": "Implements the feature.",
		"source":      "mill/g1/feature",
		"destination": "main",
		"draft":       true,
	}
	for k, w := range want {
		if checks[k] != w {
			t.Errorf("POST %s = %v, want %v (body %v)", k, checks[k], w, p.Body)
		}
	}
	if n := len(s.bbOnly(http.MethodPut)); n != 0 {
		t.Fatalf("%d PUTs, want none", n)
	}
}

func TestBitbucketUpdatesExistingPRWithoutDuplicate(t *testing.T) {
	bbRequireTools(t)
	s := bbNewServer(t, "acme", "widgets")
	s.bbSet(func() { s.open["mill/g1/feature"] = 7 })
	res := bbRunHook(t, t.TempDir(), bbInput(t, map[string]any{"title": "New title"}), bbEnv(s)...)
	bbExpectSuccess(t, res, s.html(7))

	if posts := s.bbOnly(http.MethodPost); len(posts) != 0 {
		t.Fatalf("POSTs = %+v with an open PR, want none", posts)
	}
	puts := s.bbOnly(http.MethodPut)
	if len(puts) != 1 || puts[0].Path != "/repositories/acme/widgets/pullrequests/7" {
		t.Fatalf("PUTs = %+v, want one to pullrequests/7", puts)
	}
	b := puts[0].Body
	if bbDig(b, "title") != "New title" || bbDig(b, "description") != "Implements the feature." ||
		bbDig(b, "destination", "branch", "name") != "main" {
		t.Fatalf("PUT body = %v, want title, description and destination", b)
	}
}

func TestBitbucketSecondRunUpdatesWhatFirstCreated(t *testing.T) {
	bbRequireTools(t)
	s := bbNewServer(t, "acme", "widgets")
	bbExpectSuccess(t, bbRunHook(t, t.TempDir(), bbInput(t, nil), bbEnv(s)...), s.html(100))
	bbExpectSuccess(t, bbRunHook(t, t.TempDir(), bbInput(t, nil), bbEnv(s)...), s.html(100))
	if posts, puts := s.bbOnly(http.MethodPost), s.bbOnly(http.MethodPut); len(posts) != 1 || len(puts) != 1 {
		t.Fatalf("POSTs %d, PUTs %d; want one create then one update", len(posts), len(puts))
	}
}

func TestBitbucketUpdateRetargetsToNewBase(t *testing.T) {
	bbRequireTools(t)
	s := bbNewServer(t, "acme", "widgets")
	s.bbSet(func() { s.open["mill/g1/feature"] = 9 })
	in := bbInput(t, map[string]any{"event": "update", "base_branch": "main", "previous_base_branch": "mill/g1/parent"})
	bbExpectSuccess(t, bbRunHook(t, t.TempDir(), in, bbEnv(s)...), s.html(9))
	puts := s.bbOnly(http.MethodPut)
	if len(puts) != 1 || bbDig(puts[0].Body, "destination", "branch", "name") != "main" {
		t.Fatalf("PUTs = %+v, want one retargeting to main", puts)
	}
}

func TestBitbucketStackedDescriptionNamesParentPR(t *testing.T) {
	bbRequireTools(t)
	s := bbNewServer(t, "acme", "widgets")
	s.bbSet(func() { s.open["mill/g1/parent"] = 42 })
	in := bbInput(t, map[string]any{"base_branch": "mill/g1/parent", "parent_review_branch": "mill/g1/parent"})
	bbExpectSuccess(t, bbRunHook(t, t.TempDir(), in, bbEnv(s)...), s.html(101))

	posts := s.bbOnly(http.MethodPost)
	if len(posts) != 1 {
		t.Fatalf("POSTs = %+v, want one", posts)
	}
	desc, _ := bbDig(posts[0].Body, "description").(string)
	if !strings.HasPrefix(desc, "Stacked on #42: merge that first") {
		t.Fatalf("description = %q, want it to start with the parent PR", desc)
	}
	if !strings.HasSuffix(desc, "Implements the feature.") {
		t.Fatalf("description = %q, want the task description after the prefix", desc)
	}
	if got := bbDig(posts[0].Body, "destination", "branch", "name"); got != "mill/g1/parent" {
		t.Fatalf("destination = %v, want the parent's review branch", got)
	}
}

func TestBitbucketResolvesWorkspaceRepoFromRemoteURL(t *testing.T) {
	bbRequireTools(t)
	for _, url := range []string{
		"git@bitbucket.org:acme/widgets.git",
		"git@bitbucket.org:acme/widgets",
		"https://bitbucket.org/acme/widgets.git",
		"https://bitbucket.org/acme/widgets",
		"https://alice@bitbucket.org/acme/widgets.git",
		"https://alice@bitbucket.org/acme/widgets",
	} {
		t.Run(url, func(t *testing.T) {
			s := bbNewServer(t, "acme", "widgets")
			res := bbRunHook(t, t.TempDir(), bbInput(t, map[string]any{"remote_url": url}), bbEnv(s)...)
			bbExpectSuccess(t, res, s.html(100))
		})
	}
}

func TestBitbucketBearerAuthWithTokenOnly(t *testing.T) {
	bbRequireTools(t)
	s := bbNewServer(t, "acme", "widgets")
	bbExpectSuccess(t, bbRunHook(t, t.TempDir(), bbInput(t, nil), bbEnv(s)...), s.html(100))
	for _, r := range s.recorded() {
		if r.Auth != "Bearer test-token" {
			t.Fatalf("%s %s Authorization = %q, want Bearer test-token", r.Method, r.Path, r.Auth)
		}
	}
}

func TestBitbucketBasicAuthWithUserAndToken(t *testing.T) {
	bbRequireTools(t)
	s := bbNewServer(t, "acme", "widgets")
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice@example.com:test-token"))
	s.bbSet(func() { s.wantAuth = wantAuth })
	res := bbRunHook(t, t.TempDir(), bbInput(t, nil), append(bbEnv(s), "BITBUCKET_USER=alice@example.com")...)
	bbExpectSuccess(t, res, s.html(100))
	reqs := s.recorded()
	if len(reqs) == 0 {
		t.Fatal("no requests recorded")
	}
	for _, r := range reqs {
		if r.Auth != wantAuth {
			t.Fatalf("%s %s Authorization = %q, want %q", r.Method, r.Path, r.Auth, wantAuth)
		}
	}
}

func TestBitbucketHookKeepsTokenOffCurlArgs(t *testing.T) {
	bbRequireTools(t)
	realCurl, err := exec.LookPath("curl")
	if err != nil {
		t.Fatal(err)
	}
	const token = `s3cr3t-"tok\en`
	for _, tc := range []struct {
		name     string
		env      []string
		wantAuth string
	}{
		{"bearer", nil, "Bearer " + token},
		{"basic", []string{"BITBUCKET_USER=alice@example.com"},
			"Basic " + base64.StdEncoding.EncodeToString([]byte("alice@example.com:"+token))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A fake curl first on PATH: records each argument on its own line, then runs the real one.
			bin := t.TempDir()
			argLog := filepath.Join(t.TempDir(), "curl-args")
			fake := fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do printf '%%s\\n' \"$a\" >>'%s'; done\nexec '%s' \"$@\"\n", argLog, realCurl)
			testenv.WriteExecutable(t, filepath.Join(bin, "curl"), []byte(fake))
			s := bbNewServer(t, "acme", "widgets")
			s.bbSet(func() {
				s.wantAuth = tc.wantAuth
				s.open["mill/g1/feature"] = 7 // exercises a PUT with a request body too
			})
			env := append([]string{
				"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
				"MILL_BITBUCKET_API=" + s.srv.URL,
				"BITBUCKET_TOKEN=" + token,
			}, tc.env...)
			bbExpectSuccess(t, bbRunHook(t, t.TempDir(), bbInput(t, nil), env...), s.html(7))

			data, err := os.ReadFile(argLog)
			if err != nil {
				t.Fatalf("fake curl was not run: %v", err)
			}
			for _, arg := range strings.Split(string(data), "\n") {
				if strings.Contains(arg, "s3cr3t") {
					t.Errorf("curl argument %q contains the token", arg)
				}
			}
			reqs := s.recorded()
			if len(reqs) != 2 {
				t.Fatalf("requests = %+v, want a lookup and an update", reqs)
			}
			for _, r := range reqs {
				if r.Auth != tc.wantAuth {
					t.Errorf("%s %s Authorization = %q, want %q", r.Method, r.Path, r.Auth, tc.wantAuth)
				}
			}
			if puts := s.bbOnly(http.MethodPut); len(puts) != 1 || bbDig(puts[0].Body, "title") != "Add the feature" {
				t.Fatalf("PUTs = %+v, want one carrying the request body", puts)
			}
		})
	}
}

func TestBitbucketMissingTokenFailsWithWhatToSet(t *testing.T) {
	bbRequireTools(t)
	s := bbNewServer(t, "acme", "widgets")
	res := bbRunHook(t, t.TempDir(), bbInput(t, nil), "MILL_BITBUCKET_API="+s.srv.URL)
	if res.Code != 1 || !strings.Contains(res.Stderr, "set BITBUCKET_TOKEN") {
		t.Fatalf("exit %d, stderr %q; want 1 and what to set", res.Code, res.Stderr)
	}
	if reqs := s.recorded(); len(reqs) != 0 {
		t.Fatalf("requests = %+v without a token, want none", reqs)
	}
}

func TestBitbucketHTTPErrorReportsStatusAndMessage(t *testing.T) {
	bbRequireTools(t)
	s := bbNewServer(t, "acme", "widgets")
	res := bbRunHook(t, t.TempDir(), bbInput(t, nil), "MILL_BITBUCKET_API="+s.srv.URL, "BITBUCKET_TOKEN=wrong")
	if res.Code == 0 {
		t.Fatal("exit 0 with a bad token, want non-zero")
	}
	if !strings.Contains(res.Stderr, "401") || !strings.Contains(res.Stderr, "Token is invalid") {
		t.Fatalf("stderr = %q, want the status and the message", res.Stderr)
	}
	if posts := s.bbOnly(http.MethodPost); len(posts) != 0 {
		t.Fatalf("POSTs = %+v after a failed lookup, want none", posts)
	}
}

// bbGitRepo makes a temporary git repository whose origin is url.
func bbGitRepo(t *testing.T, url string) string {
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

func TestBitbucketCheckModeSucceedsWithRepositoryAccess(t *testing.T) {
	bbRequireTools(t, "git")
	s := bbNewServer(t, "acme", "widgets")
	dir := bbGitRepo(t, "https://alice@bitbucket.org/acme/widgets.git")
	res := bbRunHook(t, dir, "", append(bbEnv(s), "MILL_HOOK_CHECK=1")...)
	if res.Code != 0 {
		t.Fatalf("exit %d, want 0\nstderr: %s", res.Code, res.Stderr)
	}
	reqs := s.recorded()
	if len(reqs) != 2 || reqs[0].Method != http.MethodGet || reqs[0].Path != "/repositories/acme/widgets" {
		t.Fatalf("requests = %+v, want GET /repositories/acme/widgets then the probe", reqs)
	}
}

func TestBitbucketCheckModeFailsWithoutRepositoryAccess(t *testing.T) {
	bbRequireTools(t, "git")
	s := bbNewServer(t, "acme", "widgets")
	s.bbSet(func() { s.noRepo = true })
	dir := bbGitRepo(t, "git@bitbucket.org:acme/widgets.git")
	res := bbRunHook(t, dir, "", append(bbEnv(s), "MILL_HOOK_CHECK=1")...)
	if res.Code == 0 {
		t.Fatal("exit 0 without repository access, want non-zero")
	}
	if !strings.Contains(res.Stderr, "404") || !strings.Contains(res.Stderr, "Repository not found") {
		t.Fatalf("stderr = %q, want the status and the message", res.Stderr)
	}
	if n := len(s.bbOnly(http.MethodPost)) + len(s.bbOnly(http.MethodPut)); n != 0 {
		t.Fatalf("check mode changed %d PRs, want none", n)
	}
}

func TestBitbucketCheckModeFailsWithoutToken(t *testing.T) {
	bbRequireTools(t, "git")
	s := bbNewServer(t, "acme", "widgets")
	dir := bbGitRepo(t, "git@bitbucket.org:acme/widgets.git")
	res := bbRunHook(t, dir, "", "MILL_BITBUCKET_API="+s.srv.URL, "MILL_HOOK_CHECK=1")
	if res.Code != 1 || !strings.Contains(res.Stderr, "set BITBUCKET_TOKEN") {
		t.Fatalf("exit %d, stderr %q; want 1 and what to set", res.Code, res.Stderr)
	}
	if reqs := s.recorded(); len(reqs) != 0 {
		t.Fatalf("requests = %+v without a token, want none", reqs)
	}
}

func TestBitbucketCheckModeFailsWithoutTools(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not found on PATH: the Bitbucket hook tests need sh")
	}
	// A PATH holding only sh: curl and jq are missing.
	bin := t.TempDir()
	if err := os.Symlink(sh, filepath.Join(bin, "sh")); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(bbScript)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, script)
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"PATH=" + bin, "BITBUCKET_TOKEN=test-token", "MILL_HOOK_CHECK=1", "TMPDIR=" + t.TempDir()}
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("check passed without curl and jq: %s", out)
	}
	if !strings.Contains(string(out), "curl not found") {
		t.Fatalf("output = %q, want the missing tool named", out)
	}
}

func TestBitbucketScriptPassesShellcheck(t *testing.T) {
	if _, err := exec.LookPath("shellcheck"); err != nil {
		t.Skip("shellcheck not found on PATH")
	}
	out, err := exec.Command("shellcheck", "-s", "sh", bbScript).CombinedOutput()
	if err != nil {
		t.Fatalf("shellcheck -s sh %s: %v\n%s", bbScript, err, out)
	}
}

// bbStatusOut is the mill/status@1 object the status event prints.
type bbStatusOut struct {
	Schema string `json:"schema"`
	State  string `json:"state"`
	URL    string `json:"url"`
	Checks []struct {
		Name    string  `json:"name"`
		URL     string  `json:"url"`
		State   string  `json:"state"`
		Summary *string `json:"summary"`
	} `json:"checks"`
}

const bbHeadSHA = "0123456789abcdef0123456789abcdef01234567"

// bbRunStatus runs the status event and decodes its stdout, which must be one JSON object.
func bbRunStatus(t *testing.T, s *bbServer) bbStatusOut {
	t.Helper()
	res := bbRunHook(t, t.TempDir(), bbInput(t, map[string]any{"event": "status"}), bbEnv(s)...)
	if res.Code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", res.Code, res.Stdout, res.Stderr)
	}
	var out bbStatusOut
	dec := json.NewDecoder(strings.NewReader(res.Stdout))
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("stdout %q is not a JSON object: %v", res.Stdout, err)
	}
	if dec.More() {
		t.Fatalf("stdout %q holds more than one JSON value", res.Stdout)
	}
	if out.Schema != "mill/status@1" {
		t.Fatalf("schema = %q, want mill/status@1", out.Schema)
	}
	if n := len(s.bbOnly(http.MethodPost)) + len(s.bbOnly(http.MethodPut)); n != 0 {
		t.Fatalf("the status event changed %d PRs, want none", n)
	}
	return out
}

func bbStatus(state, key, name, url, desc string) map[string]any {
	st := map[string]any{"state": state, "key": key, "url": url, "description": desc}
	if name != "" {
		st["name"] = name
	}
	return st
}

func TestBbStatusFailedGivesFailureWithNameURLAndSummary(t *testing.T) {
	bbRequireTools(t)
	s := bbNewServer(t, "acme", "widgets")
	s.bbSet(func() { s.open["mill/g1/feature"] = 7 })
	s.bbSet(func() {
		s.statuses[bbHeadSHA] = []map[string]any{
			bbStatus("SUCCESSFUL", "lint", "Lint", "https://ci.example/lint", "ok"),
			bbStatus("FAILED", "unit", "Unit tests", "https://ci.example/unit", "3 tests failed"),
			bbStatus("INPROGRESS", "e2e", "", "https://ci.example/e2e", "running"),
		}
	})
	out := bbRunStatus(t, s)
	if out.State != "failure" {
		t.Fatalf("state = %q, want failure", out.State)
	}
	if out.URL != s.html(7) {
		t.Fatalf("url = %q, want the open PR's link %q", out.URL, s.html(7))
	}
	if len(out.Checks) != 3 {
		t.Fatalf("checks = %+v, want 3", out.Checks)
	}
	f := out.Checks[1]
	if f.Name != "Unit tests" || f.URL != "https://ci.example/unit" || f.State != "failure" ||
		f.Summary == nil || *f.Summary != "3 tests failed" {
		t.Fatalf("failed check = %+v, want name, url, failure and the description as summary", f)
	}
	if c := out.Checks[0]; c.State != "success" || c.Summary != nil {
		t.Fatalf("successful check = %+v, want success without a summary", c)
	}
	if c := out.Checks[2]; c.Name != "e2e" || c.State != "pending" {
		t.Fatalf("in-progress check = %+v, want the key as name and pending", c)
	}
}

func TestBbStatusStoppedIsFailureAndSummaryIsCut(t *testing.T) {
	bbRequireTools(t)
	s := bbNewServer(t, "acme", "widgets")
	long := strings.Repeat("é", 2000) // 4000 bytes
	s.bbSet(func() {
		s.statuses[bbHeadSHA] = []map[string]any{bbStatus("STOPPED", "build", "Build", "https://ci.example/b", long)}
	})
	out := bbRunStatus(t, s)
	if out.State != "failure" || len(out.Checks) != 1 || out.Checks[0].Summary == nil {
		t.Fatalf("status = %+v, want one failed check with a summary", out)
	}
	sum := *out.Checks[0].Summary
	if len(sum) > 2048 || len(sum) < 2046 || !strings.HasPrefix(long, sum) {
		t.Fatalf("summary is %d bytes, want the description cut to at most 2048 on a character boundary", len(sum))
	}
	if out.URL != "" {
		t.Fatalf("url = %q without an open PR, want none", out.URL)
	}
}

func TestBbStatusInProgressAndSuccessfulGivesPending(t *testing.T) {
	bbRequireTools(t)
	s := bbNewServer(t, "acme", "widgets")
	s.bbSet(func() {
		s.statuses[bbHeadSHA] = []map[string]any{
			bbStatus("SUCCESSFUL", "lint", "Lint", "https://ci.example/lint", ""),
			bbStatus("INPROGRESS", "unit", "Unit", "https://ci.example/unit", ""),
		}
	})
	if out := bbRunStatus(t, s); out.State != "pending" {
		t.Fatalf("state = %q, want pending", out.State)
	}
}

func TestBbStatusAllSuccessfulGivesSuccess(t *testing.T) {
	bbRequireTools(t)
	s := bbNewServer(t, "acme", "widgets")
	s.bbSet(func() {
		s.statuses[bbHeadSHA] = []map[string]any{
			bbStatus("SUCCESSFUL", "lint", "Lint", "https://ci.example/lint", ""),
			bbStatus("SUCCESSFUL", "unit", "Unit", "https://ci.example/unit", ""),
		}
	})
	out := bbRunStatus(t, s)
	if out.State != "success" || len(out.Checks) != 2 {
		t.Fatalf("status = %+v, want success with two checks", out)
	}
}

func TestBbStatusNoStatusesGivesNone(t *testing.T) {
	bbRequireTools(t)
	s := bbNewServer(t, "acme", "widgets")
	out := bbRunStatus(t, s)
	if out.State != "none" || len(out.Checks) != 0 {
		t.Fatalf("status = %+v, want none with no checks", out)
	}
}

func TestBbStatusFollowsNextPages(t *testing.T) {
	bbRequireTools(t)
	s := bbNewServer(t, "acme", "widgets")
	s.bbSet(func() { s.pageSize = 2 })
	s.bbSet(func() {
		s.statuses[bbHeadSHA] = []map[string]any{
			bbStatus("SUCCESSFUL", "a", "A", "https://ci.example/a", ""),
			bbStatus("SUCCESSFUL", "b", "B", "https://ci.example/b", ""),
			bbStatus("SUCCESSFUL", "c", "C", "https://ci.example/c", ""),
			bbStatus("FAILED", "d", "D", "https://ci.example/d", "broken"),
			bbStatus("SUCCESSFUL", "e", "E", "https://ci.example/e", ""),
		}
	})
	out := bbRunStatus(t, s)
	if out.State != "failure" || len(out.Checks) != 5 {
		t.Fatalf("status = %+v, want all 5 checks across pages and failure", out)
	}
}

func TestBbStatusFailsWhenStatusesAreRefused(t *testing.T) {
	bbRequireTools(t)
	s := bbNewServer(t, "acme", "widgets")
	s.bbSet(func() { s.statusesStatus = http.StatusForbidden })
	res := bbRunHook(t, t.TempDir(), bbInput(t, map[string]any{"event": "status"}), bbEnv(s)...)
	if res.Code == 0 || res.Stdout != "" || !strings.Contains(res.Stderr, "403") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want a failure naming 403 and no output", res.Code, res.Stdout, res.Stderr)
	}
}

// bbRunCheck runs check mode in a temporary repository whose origin is the fake repository.
func bbRunCheck(t *testing.T, s *bbServer, env ...string) bbResult {
	t.Helper()
	dir := bbGitRepo(t, "git@bitbucket.org:acme/widgets.git")
	return bbRunHook(t, dir, "", append(append(bbEnv(s), "MILL_HOOK_CHECK=1"), env...)...)
}

func TestBbCheckPassesOnProbeAnswered400(t *testing.T) {
	bbRequireTools(t, "git")
	s := bbNewServer(t, "acme", "widgets")
	res := bbRunCheck(t, s)
	if res.Code != 0 {
		t.Fatalf("exit %d, want 0\nstderr: %s", res.Code, res.Stderr)
	}
	posts := s.bbOnly(http.MethodPost)
	if len(posts) != 1 || posts[0].Path != "/repositories/acme/widgets/pullrequests" || len(posts[0].Body) != 0 {
		t.Fatalf("POSTs = %+v, want one empty {} to pullrequests", posts)
	}
	if open := s.bbOpenPRs(); len(open) != 0 {
		t.Fatalf("check mode opened PRs %v, want none", open)
	}
}

func TestBbCheckFailsOnProbeRefusedWithWhatToGrant(t *testing.T) {
	bbRequireTools(t, "git")
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			s := bbNewServer(t, "acme", "widgets")
			s.bbSet(func() { s.probeStatus = code })
			res := bbRunCheck(t, s)
			want := "the token cannot create pull requests: give it Pull requests: Write on acme/widgets"
			if res.Code == 0 || !strings.Contains(res.Stderr, want) {
				t.Fatalf("exit %d, stderr %q; want a failure saying %q", res.Code, res.Stderr, want)
			}
		})
	}
}

func TestBbCheckFailsOnOtherProbeAnswerWithTheCode(t *testing.T) {
	bbRequireTools(t, "git")
	s := bbNewServer(t, "acme", "widgets")
	s.bbSet(func() { s.probeStatus = http.StatusInternalServerError })
	res := bbRunCheck(t, s)
	if res.Code == 0 || !strings.Contains(res.Stderr, "500") {
		t.Fatalf("exit %d, stderr %q; want a failure naming 500", res.Code, res.Stderr)
	}
}

func TestBbCheckStatusEventReadsDefaultBranchStatuses(t *testing.T) {
	bbRequireTools(t, "git")
	s := bbNewServer(t, "acme", "widgets")
	res := bbRunCheck(t, s, "MILL_HOOK_EVENT=status")
	if res.Code != 0 {
		t.Fatalf("exit %d, want 0\nstderr: %s", res.Code, res.Stderr)
	}
	var found bool
	for _, r := range s.bbOnly(http.MethodGet) {
		if r.Path == "/repositories/acme/widgets/commit/main/statuses" {
			found = true
		}
	}
	if !found {
		t.Fatalf("requests = %+v, want a GET of the main branch statuses", s.recorded())
	}
}

func TestBbCheckStatusEventFailsWhenDefaultBranchStatusesRefused(t *testing.T) {
	bbRequireTools(t, "git")
	s := bbNewServer(t, "acme", "widgets")
	s.bbSet(func() { s.statusesStatus = http.StatusForbidden })
	res := bbRunCheck(t, s, "MILL_HOOK_EVENT=status")
	if res.Code == 0 || !strings.Contains(res.Stderr, "Repositories: Read") ||
		!strings.Contains(res.Stderr, "Pipelines: Read") || !strings.Contains(res.Stderr, "403") {
		t.Fatalf("exit %d, stderr %q; want a failure naming 403 and what to grant", res.Code, res.Stderr)
	}
}

func TestBbCheckWithoutStatusEventSkipsStatuses(t *testing.T) {
	bbRequireTools(t, "git")
	s := bbNewServer(t, "acme", "widgets")
	s.bbSet(func() { s.statusesStatus = http.StatusForbidden })
	if res := bbRunCheck(t, s); res.Code != 0 {
		t.Fatalf("exit %d, want 0 when statuses are not needed\nstderr: %s", res.Code, res.Stderr)
	}
}

func TestBbDraftDefaultsToTrue(t *testing.T) {
	bbRequireTools(t)
	s := bbNewServer(t, "acme", "widgets")
	bbExpectSuccess(t, bbRunHook(t, t.TempDir(), bbInput(t, nil), bbEnv(s)...), s.html(100))
	posts := s.bbOnly(http.MethodPost)
	if len(posts) != 1 || bbDig(posts[0].Body, "draft") != true {
		t.Fatalf("POSTs = %+v, want one with draft true", posts)
	}
}

func TestBbDraftZeroCreatesNormalPR(t *testing.T) {
	bbRequireTools(t)
	s := bbNewServer(t, "acme", "widgets")
	res := bbRunHook(t, t.TempDir(), bbInput(t, nil), append(bbEnv(s), "MILL_BITBUCKET_DRAFT=0")...)
	bbExpectSuccess(t, res, s.html(100))
	posts := s.bbOnly(http.MethodPost)
	if len(posts) != 1 || bbDig(posts[0].Body, "draft") != false {
		t.Fatalf("POSTs = %+v, want one with draft false", posts)
	}
}
