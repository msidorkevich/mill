package doctor

import (
	"encoding/json"
	"strings"
	"testing"
)

// statusHookEventScript exits 0 only when run in check mode for event.
func statusHookEventScript(event string) string {
	return `[ "$MILL_HOOK_EVENT" = ` + event + ` ] && [ "$MILL_HOOK_CHECK" = 1 ] || { echo "event=$MILL_HOOK_EVENT check=$MILL_HOOK_CHECK" >&2; exit 1; }`
}

func statusHookCount(r Report, name string) int {
	n := 0
	for _, c := range r.Checks {
		if c.Name == name {
			n++
		}
	}
	return n
}

func TestDoctorStatusHookOKWhenScriptSeesStatusEvent(t *testing.T) {
	t.Parallel()
	r := runHookDoctor(hookRepo(t, "status = \"./hooks/hook.sh\"\n", statusHookEventScript("status")))
	c := byName(r)["hook_status"]
	if !r.OK || c.Status != OK {
		t.Fatalf("report = %+v, want hook_status ok", r)
	}
}

func TestDoctorStatusHookPublishAndUpdateReceiveTheirEvent(t *testing.T) {
	t.Parallel()
	// Accepts only publish or update events, so both checks pass only if each gets its own event.
	script := `case "$MILL_HOOK_EVENT" in publish|update) [ "$MILL_HOOK_CHECK" = 1 ] || exit 1 ;; *) exit 1 ;; esac`
	r := runHookDoctor(hookRepo(t, "on_publish = \"./hooks/hook.sh\"\non_update = \"./hooks/hook.sh\"\n", script))
	c := byName(r)
	if !r.OK || c["on_publish"].Status != OK || c["on_update"].Status != OK {
		t.Fatalf("report = %+v, want both hooks ok", r)
	}
}

func TestDoctorStatusHookFailureReportsOutputTail(t *testing.T) {
	t.Parallel()
	r := runHookDoctor(hookRepo(t, "status = \"./hooks/hook.sh\"\n", "echo status endpoint down >&2\nexit 1"))
	c := byName(r)["hook_status"]
	if r.OK || c.Status != Fail {
		t.Fatalf("report = %+v, want hook_status failure", r)
	}
	for _, want := range []string{"hook hook_status", "exit 1", "status endpoint down"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail %q lacks %q", c.Detail, want)
		}
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"name":"hook_status","status":"fail"`, "status endpoint down", `"ok":false`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("JSON %s lacks %q", b, want)
		}
	}
}

func TestDoctorStatusHookAbsentWhenUnset(t *testing.T) {
	t.Parallel()
	r := runHookDoctor(hookRepo(t, "on_publish = \"./hooks/hook.sh\"\n", "exit 0"))
	if statusHookCount(r, "hook_status") != 0 {
		t.Fatalf("report = %+v, want no hook_status check", r)
	}
}

func TestDoctorStatusHookSameCommandAsPublishRunsOncePerKey(t *testing.T) {
	t.Parallel()
	r := runHookDoctor(hookRepo(t, "on_publish = \"./hooks/hook.sh\"\nstatus = \"./hooks/hook.sh\"\n", statusHookEventScript("publish")))
	if statusHookCount(r, "on_publish") != 1 || statusHookCount(r, "hook_status") != 1 {
		t.Fatalf("report = %+v, want one on_publish and one hook_status check", r)
	}
	c := byName(r)
	if c["on_publish"].Status != OK || c["hook_status"].Status != Fail {
		t.Fatalf("report = %+v, want on_publish ok and hook_status fail (script only accepts publish)", r)
	}
}
