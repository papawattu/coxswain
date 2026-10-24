package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/papawattu/coxswain/pkg/memkey"
)

type fakeMemoryAdmin struct {
	active  []string
	listErr error
	delErr  error
	deleted []string
}

func (f *fakeMemoryAdmin) ActiveLoops(context.Context, string) ([]string, error) {
	return f.active, f.listErr
}

func (f *fakeMemoryAdmin) DeletePVC(_ context.Context, name string) error {
	f.deleted = append(f.deleted, name)
	return f.delErr
}

// TestMemoryDeleteRefusedWhileLoopActive (I101): an active Loop on the repo
// makes delete fail naming the Loop, and the PVC is NOT deleted.
func TestMemoryDeleteRefusedWhileLoopActive(t *testing.T) {
	a := &fakeMemoryAdmin{active: []string{"fix-1 (Implementing)"}}
	out := &strings.Builder{}
	err := memoryDelete(context.Background(), Deps{MemoryAdmin: a}, "ns1", memTestRepo, out)
	if err == nil || !strings.Contains(err.Error(), "refusing") || !strings.Contains(err.Error(), "fix-1 (Implementing)") {
		t.Fatalf("want a refusal naming the Loop, got %v", err)
	}
	if len(a.deleted) != 0 {
		t.Fatalf("PVC deleted despite an active Loop: %v", a.deleted)
	}
}

// TestMemoryDeleteDeletesPVC (I101): no active Loop deletes the repo's PVC.
func TestMemoryDeleteDeletesPVC(t *testing.T) {
	a := &fakeMemoryAdmin{}
	out := &strings.Builder{}
	if err := memoryDelete(context.Background(), Deps{MemoryAdmin: a}, "ns1", memTestRepo, out); err != nil {
		t.Fatal(err)
	}
	if len(a.deleted) != 1 || a.deleted[0] != memkey.PVCName(memTestRepo) {
		t.Fatalf("deleted %v", a.deleted)
	}
	if !strings.Contains(out.String(), "Deleted memory PVC") {
		t.Fatalf("output: %s", out.String())
	}
}

// TestMemoryDeleteMissingPVC: nothing to delete is a note, not an error.
func TestMemoryDeleteMissingPVC(t *testing.T) {
	a := &fakeMemoryAdmin{delErr: memoryNotFoundErr{}}
	out := &strings.Builder{}
	if err := memoryDelete(context.Background(), Deps{MemoryAdmin: a}, "ns1", memTestRepo, out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nothing to delete") {
		t.Fatalf("output: %s", out.String())
	}
}

// TestActiveLoopsFromJSON: only non-terminal Loops on the exact repo count;
// Succeeded/Failed and other repos do not; an unset phase is active.
func TestActiveLoopsFromJSON(t *testing.T) {
	js := `{"items":[
	 {"metadata":{"name":"a"},"spec":{"workspace":{"repo":"` + memTestRepo + `"}},"status":{"phase":"Succeeded","conditions":[{"type":"MemoryWriteDone","status":"True"}]}},
	 {"metadata":{"name":"b"},"spec":{"workspace":{"repo":"` + memTestRepo + `"}},"status":{"phase":"Failed"}},
	 {"metadata":{"name":"c"},"spec":{"workspace":{"repo":"` + memTestRepo + `"}},"status":{"phase":"Paused"}},
	 {"metadata":{"name":"d"},"spec":{"workspace":{"repo":"` + memTestRepo + `"}},"status":{}},
	 {"metadata":{"name":"e"},"spec":{"workspace":{"repo":"https://other/x.git"}},"status":{"phase":"Implementing"}}]}`
	got, err := activeLoopsFromJSON([]byte(js), memTestRepo)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "c (Paused),d (Pending)" {
		t.Fatalf("got %v", got)
	}
}

// TestActiveLoopsFromJSONWriterPending (I101 follow-up): a terminal Loop whose
// memory writer has not finished (no MemoryWriteDone condition) is active, if
// it qualifies for a write; a terminal Loop with the condition, or one that
// never qualifies (Failed, other reason), is not.
func TestActiveLoopsFromJSONWriterPending(t *testing.T) {
	loop := func(name, phase, conds string) string {
		return `{"metadata":{"name":"` + name + `"},"spec":{"workspace":{"repo":"` + memTestRepo +
			`"}},"status":{"phase":"` + phase + `","conditions":[` + conds + `]}}`
	}
	done := `{"type":"MemoryWriteDone","status":"True","reason":"MemoryWritten"}`
	stalled := `{"type":"Failed","status":"True","reason":"Stalled"}`
	budget := `{"type":"Failed","status":"True","reason":"BudgetExceeded"}`
	js := `{"items":[` + strings.Join([]string{
		loop("ok-pending", "Succeeded", ""),
		loop("ok-done", "Succeeded", done),
		loop("stall-pending", "Failed", stalled),
		loop("stall-done", "Failed", stalled+","+done),
		loop("budget", "Failed", budget),
	}, ",") + `]}`
	got, err := activeLoopsFromJSON([]byte(js), memTestRepo)
	if err != nil {
		t.Fatal(err)
	}
	want := "ok-pending (Succeeded, memory write pending),stall-pending (Failed, memory write pending)"
	if strings.Join(got, ",") != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
