package tui

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A review that parked on incomplete coverage shows no finding, so `f` used to
// send a fix with nothing to fix. On that gate the same key asks for the rest
// of the review (types.ActionRereview) and the action bar says so.
func TestModel_FixKeyOnACoverageParkedReviewSendsARereview(t *testing.T) {
	for _, status := range []types.StepStatus{types.StepStatusAwaitingApproval, types.StepStatusFixReview} {
		t.Run(string(status), func(t *testing.T) {
			sock, client, snapshot := captureRespond(t)
			run := testRun()
			fj := `{"findings":[],"summary":"clean","reviewed_paths":["a.go"]}`
			run.Steps = []ipc.StepResultInfo{{StepName: types.StepReview, Status: status, FindingsJSON: &fj}}
			m := NewModel(sock, client, run)
			m.stepDiffLoaded[types.StepReview] = true

			_, allowFix, _, total := m.awaitingActionState()
			if !allowFix || total != 0 {
				t.Fatalf("awaitingActionState allowFix=%v total=%d, want the rereview offered on a zero-finding coverage gate", allowFix, total)
			}
			bar := renderApprovalActions(false, allowFix, false, 0, total, false, false, true, false)
			if !strings.Contains(bar, "rereview") {
				t.Fatalf("action bar %q does not offer the rereview", bar)
			}

			cmd := m.respondCmd(types.ActionFix)
			if cmd == nil {
				t.Fatal("f on a coverage gate produced no response")
			}
			if msg := cmd(); msg != nil {
				t.Fatalf("respond failed: %v", msg)
			}
			calls := snapshot()
			if len(calls) != 1 || calls[0].Action != types.ActionRereview || calls[0].Step != types.StepReview {
				t.Fatalf("responses = %+v, want exactly one rereview of the review step", calls)
			}
			if len(calls[0].FindingIDs) != 0 || len(calls[0].IgnoreFindingIDs) != 0 {
				t.Fatalf("a rereview carries no finding selection: %+v", calls[0])
			}
		})
	}
}

// Only the coverage gate is remapped: a zero-finding gate on any other step
// keeps today's behavior, and a review gate with a blocking finding is still
// fixed through its selection.
func TestModel_FixKeyOutsideACoverageGateIsNotARereview(t *testing.T) {
	sock, client, snapshot := captureRespond(t)
	run := testRun()
	fj := `{"findings":[{"id":"review-1","severity":"warning","description":"bug","action":"auto-fix"}],"summary":"1 issue"}`
	run.Steps = []ipc.StepResultInfo{{StepName: types.StepReview, Status: types.StepStatusAwaitingApproval, FindingsJSON: &fj}}
	m := NewModel(sock, client, run)
	m.stepDiffLoaded[types.StepReview] = true

	cmd := m.respondCmd(types.ActionFix)
	if cmd == nil {
		t.Fatal("f with a selectable finding produced no response")
	}
	if msg := cmd(); msg != nil {
		t.Fatalf("respond failed: %v", msg)
	}
	calls := snapshot()
	if len(calls) != 1 || calls[0].Action != types.ActionFix {
		t.Fatalf("responses = %+v, want an ordinary fix", calls)
	}
}
