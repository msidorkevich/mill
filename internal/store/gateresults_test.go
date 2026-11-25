package store

import (
	"context"
	"path/filepath"
	"testing"
)

func gateReuseStore(t *testing.T) *Store {
	t.Helper()
	return open(t, filepath.Join(t.TempDir(), "mill.db"))
}

func TestGateReuseRecordedPassIsFoundForSameTreeAndChecks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := gateReuseStore(t)
	fp := GateFingerprint("make check", []string{"go test ./b", "go test ./a"})
	if err := s.RecordGatePass(ctx, "tree1", fp, "G-1/a", []byte(`{"passed":true}`)); err != nil {
		t.Fatalf("RecordGatePass: %v", err)
	}
	r, ok, err := s.PassedGate(ctx, "tree1", GateFingerprint("make check", []string{"go test ./a", "go test ./b"}))
	if err != nil || !ok {
		t.Fatalf("PassedGate = %v, %v; want a hit (checks in another order)", ok, err)
	}
	if r.TaskID != "G-1/a" || string(r.Result) != `{"passed":true}` || !r.PassedAt.Equal(fixed) || r.TreeSHA != "tree1" {
		t.Fatalf("PassedGate = %+v", r)
	}
}

func TestGateReuseMissesOnOtherTreeOrChangedChecks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := gateReuseStore(t)
	fp := GateFingerprint("make check", []string{"go test ./a"})
	if err := s.RecordGatePass(ctx, "tree1", fp, "G-1/a", []byte(`{"passed":true}`)); err != nil {
		t.Fatalf("RecordGatePass: %v", err)
	}
	for name, k := range map[string][2]string{
		"other tree":      {"tree2", fp},
		"other check_cmd": {"tree1", GateFingerprint("make test", []string{"go test ./a"})},
		"changed check":   {"tree1", GateFingerprint("make check", []string{"go test ./b"})},
		"added check":     {"tree1", GateFingerprint("make check", []string{"go test ./a", "go vet ./..."})},
		"dropped check":   {"tree1", GateFingerprint("make check", nil)},
	} {
		if _, ok, err := s.PassedGate(ctx, k[0], k[1]); err != nil || ok {
			t.Errorf("%s: PassedGate = %v, %v; want a miss", name, ok, err)
		}
	}
}

func TestGateReuseFingerprintSeparatesCheckCmdFromChecks(t *testing.T) {
	t.Parallel()
	if GateFingerprint("a", []string{"b"}) == GateFingerprint("", []string{"a", "b"}) {
		t.Fatal("check_cmd a + check b fingerprints like no check_cmd + checks a, b")
	}
	if GateFingerprint("x", []string{"a", " "}) != GateFingerprint("x", []string{"a"}) {
		t.Fatal("a blank check changed the fingerprint")
	}
}

func TestGateReuseRecordRequiresTreeAndFingerprint(t *testing.T) {
	t.Parallel()
	s := gateReuseStore(t)
	if err := s.RecordGatePass(context.Background(), "", "fp", "G-1/a", nil); err == nil {
		t.Fatal("RecordGatePass without a tree succeeded")
	}
}
