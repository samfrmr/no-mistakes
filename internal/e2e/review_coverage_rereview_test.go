//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// reviewCoverageScenario scripts a reviewer that finds nothing but whose
// coverage record is the barmkin symptom: the first turn records no coverage
// at all (reviewed_paths: [] - the reviewer "exits after one message"), and the
// focused coverage pass that runs before parking verifies only the first file.
// A later focused pass, the one an operator's rereview triggers, lists only the
// file still unverified, which is how the two are told apart: the first lists
// both files, the rereview lists the remainder alone. lateCovers decides
// whether that later pass actually verifies it.
func reviewCoverageScenario(t *testing.T, lateCovers bool) string {
	t.Helper()
	late := "[]"
	if lateCovers {
		late = "[policy.yaml]"
	}
	body := fmt.Sprintf(`actions:
  - match: "  - root_clear.go"
    structured:
      findings: []
      summary: "partial"
      risk_level: low
      risk_rationale: "read one file"
      risk_scope: source-or-external
      reviewed_paths: [root_clear.go]
  - match: "Focused coverage completion:"
    structured:
      findings: []
      summary: "focused"
      risk_level: low
      risk_rationale: "read the remaining file"
      risk_scope: source-or-external
      reviewed_paths: %s
  - match: "Review the code changes and return structured findings"
    structured:
      findings: []
      summary: "reviewer exited after one message"
      risk_level: low
      risk_rationale: "no coverage recorded"
      risk_scope: source-or-external
      reviewed_paths: []
  - structured:
      findings: []
      summary: "clean"
      risk_level: low
      risk_rationale: "none"
      risk_scope: source-or-external
      tested: ["fixture"]
      testing_summary: "fixture"
      scenarios:
        - name: "fixture"
          result: pass
          live: true
          evidence: "fixture"
          reason: ""
      verdict: go
      artifacts: []
      title: "fix: root clear"
      body: "Root clear"
`, late)
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func startCoverageParkedRun(t *testing.T, lateCovers bool) (*Harness, string) {
	t.Helper()
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: reviewCoverageScenario(t, lateCovers)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	branch := "feature/root-clear"
	h.CommitChange(branch, "root_clear.go", "package main\n", "root clear")
	h.CommitChange(branch, "policy.yaml", "allow: []\n", "policy yaml")
	out, err := h.Run("axi", "run", "--intent", "Add the root clear command")
	if err != nil {
		t.Fatalf("axi run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "gate:") || !strings.Contains(out, "step: review") {
		t.Fatalf("the run did not park at the review gate:\n%s", out)
	}
	return h, branch
}

// A review that verified only part of the change parks with no finding.
// `fix` refuses it, the gate's help leads with the rereview instead of an
// approval, and a rereview whose focused pass still leaves a file unverified
// re-parks rather than advancing.
func TestReviewCoverageGateRereviewParksUntilCoverageIsComplete(t *testing.T) {
	h, branch := startCoverageParkedRun(t, false)

	status, err := h.Run("axi", "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, status)
	}
	if !strings.Contains(status, "--action rereview") {
		t.Fatalf("the parked gate does not offer the rereview:\n%s", status)
	}
	t.Logf("parked coverage gate:\n%s", status)
	out, err := h.Run("axi", "respond", "--action", "fix")
	if err == nil || !strings.Contains(out, "--action fix requires") {
		t.Fatalf("fix with nothing to fix must still be refused (err=%v):\n%s", err, out)
	}
	t.Logf("fix on the coverage gate:\n%s", out)

	out, err = h.Run("axi", "respond", "--action", "rereview")
	if err != nil {
		t.Fatalf("rereview: %v\n%s", err, out)
	}
	t.Logf("rereview that left a file unverified:\n%s", out)
	if !strings.Contains(out, "gate:") || !strings.Contains(out, "step: review") {
		t.Fatalf("a rereview that left policy.yaml unverified must re-park at the review gate:\n%s", out)
	}
	run := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
	if run.Status == types.RunCompleted {
		t.Fatalf("the run advanced without complete coverage: %+v", run)
	}
	if log, err := h.Run("axi", "logs", "--step", "review", "--full"); err != nil || !strings.Contains(log, "unverified: policy.yaml") {
		t.Fatalf("the re-park must name the file still unverified (err=%v):\n%s", err, log)
	}
	if out, err := h.Run("axi", "abort"); err != nil {
		t.Fatalf("abort: %v\n%s", err, out)
	}
}

// The same gate advances once the rereview's focused pass verifies the
// remaining file: the review completes on a complete record, with no waiver.
func TestReviewCoverageGateRereviewAdvancesOnceCoverageIsComplete(t *testing.T) {
	h, branch := startCoverageParkedRun(t, true)

	out, err := h.Run("axi", "respond", "--action", "rereview")
	if err != nil {
		t.Fatalf("rereview: %v\n%s", err, out)
	}
	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("a rereview that completed the coverage did not advance the run: status=%s error=%v\n%s", run.Status, run.Error, out)
	}
	review, ok := findStep(run.Steps, types.StepReview)
	if !ok || review.Status != types.StepStatusCompleted {
		t.Fatalf("review step = %+v, want completed by coverage and not skipped", review)
	}
}

func runCoverageBranchWithYes(t *testing.T, lateCovers bool) (*Harness, string, string) {
	t.Helper()
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: reviewCoverageScenario(t, lateCovers)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	branch := "feature/root-clear"
	h.CommitChange(branch, "root_clear.go", "package main\n", "root clear")
	h.CommitChange(branch, "policy.yaml", "allow: []\n", "policy yaml")
	out, err := h.Run("axi", "run", "--yes", "--intent", "Add the root clear command")
	if err != nil {
		t.Fatalf("axi run --yes: %v\n%s", err, out)
	}
	return h, branch, out
}

// --yes is standing consent to fix findings, never to waive coverage: when the
// rereview it issues leaves a file unverified, the run stays parked at the
// review gate instead of approving it.
func TestReviewCoverageYesNeverApprovesAnIncompleteReview(t *testing.T) {
	h, branch, out := runCoverageBranchWithYes(t, false)
	if !strings.Contains(out, "gate:") || !strings.Contains(out, "step: review") {
		t.Fatalf("--yes did not stop at the still-incomplete review gate:\n%s", out)
	}
	run := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
	if run.Status == types.RunCompleted {
		t.Fatalf("--yes advanced the run past an incomplete review: %+v", run)
	}
	if out, err := h.Run("axi", "abort"); err != nil {
		t.Fatalf("abort: %v\n%s", err, out)
	}
}

// ...and when the single rereview --yes issues does complete the coverage, the
// run advances on a complete record.
func TestReviewCoverageYesAdvancesOnceTheRereviewCompletesCoverage(t *testing.T) {
	h, branch, out := runCoverageBranchWithYes(t, true)
	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("--yes with a completing rereview did not finish the run: status=%s error=%v\n%s", run.Status, run.Error, out)
	}
}
