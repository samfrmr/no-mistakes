package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A review whose reviewed_paths record leaves reviewable files unverified
// parks with ZERO findings: there is no defect, only missing evidence. `fix`
// refuses such a gate (nothing to select) and approve waives the unread files,
// so before types.ActionRereview the operator's only supported moves were to
// waive the review or abandon the run. These tests pin the supported way
// forward at the executor: the response re-enters the review step with the
// parked record, records no selection, and the step completes only when the
// record it returns is complete.

const coveragePark = `{"findings":[],"summary":"clean","reviewed_paths":["a.go"],"risk_level":"low","risk_rationale":"clean"}`

// coveragePartialOutcome is the outcome ReviewStep returns for a clean round
// whose record covers a.go but not b.go.
func coveragePartialOutcome() *StepOutcome {
	return &StepOutcome{
		NeedsApproval:   true,
		Findings:        coveragePark,
		ReviewedPaths:   []string{"a.go"},
		ReviewablePaths: []string{"a.go", "b.go"},
	}
}

func coverageCompleteOutcome() *StepOutcome {
	return &StepOutcome{
		Findings:        `{"findings":[],"summary":"clean","reviewed_paths":["a.go","b.go"],"risk_level":"low","risk_rationale":"clean"}`,
		ReviewedPaths:   []string{"a.go", "b.go"},
		ReviewablePaths: []string{"a.go", "b.go"},
	}
}

func TestExecutor_RereviewExtendsAParkedCoveragePark(t *testing.T) {
	database, p, run, repo := setupTest(t)
	calls := 0
	var second struct {
		coverage   string
		finalizing bool
		fixing     bool
	}
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			calls++
			if calls == 1 {
				return coveragePartialOutcome(), nil
			}
			second.coverage = sctx.CoverageFindings
			second.finalizing = sctx.FinalizingAnswers
			second.fixing = sctx.Fixing
			return coverageCompleteOutcome(), nil
		},
	}
	exec := NewExecutor(database, p, &config.Config{}, newFakeSessionAgent(), []Step{step}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	if err := exec.Respond(types.StepReview, types.ActionRereview, nil); err != nil {
		t.Fatalf("Respond(rereview) error = %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run never finished after a rereview that completed the coverage")
	}

	if calls != 2 {
		t.Fatalf("review step ran %d times, want the parked round plus one extension", calls)
	}
	if second.coverage != coveragePark && !strings.Contains(second.coverage, `"reviewed_paths":["a.go"]`) {
		t.Fatalf("the extension did not receive the parked record: %q", second.coverage)
	}
	if second.finalizing || second.fixing {
		t.Fatalf("a rereview is neither an answer round nor a fix round (finalizing=%v fixing=%v)", second.finalizing, second.fixing)
	}

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := steps[0].Status; got != types.StepStatusCompleted {
		t.Fatalf("review step status = %s, want completed once the record is complete", got)
	}
	rounds, err := database.GetRoundsByStep(steps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 2 || rounds[1].Trigger != "rereview" {
		t.Fatalf("rounds = %+v, want the parked round then a rereview round", rounds)
	}
	if rounds[1].IsFixRound() {
		t.Fatal("a rereview round reads as a fix round")
	}
	if rounds[0].SelectedFindingIDs != nil {
		t.Fatalf("the parked round records selection %q: a rereview decides nothing about its findings", *rounds[0].SelectedFindingIDs)
	}
}

// The extension is not an approval: a record that still leaves a file
// unverified parks again, and the operator can ask again. Only a complete
// record completes the step.
func TestExecutor_RereviewThatStaysIncompleteParksAgain(t *testing.T) {
	database, p, run, repo := setupTest(t)
	calls := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			calls++
			if calls < 3 {
				return coveragePartialOutcome(), nil
			}
			return coverageCompleteOutcome(), nil
		},
	}
	exec := NewExecutor(database, p, &config.Config{}, newFakeSessionAgent(), []Step{step}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	if err := exec.Respond(types.StepReview, types.ActionRereview, nil); err != nil {
		t.Fatalf("first rereview: %v", err)
	}
	// Re-parked: the same gate accepts a second rereview, and the run has not
	// advanced past the review in between.
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := exec.Respond(types.StepReview, types.ActionRereview, nil)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the re-parked gate never accepted a second rereview: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not finish after the second rereview")
	}
	if calls != 3 {
		t.Fatalf("review step ran %d times, want 3 (park, still-incomplete rereview, completing rereview)", calls)
	}
}

// A gate that carries a blocking finding is decided by the response that
// finding asks for. Re-reviewing there would discard the parked pass, so the
// response is refused and the gate stays parked for a valid one.
func TestExecutor_RereviewIsRefusedWhenTheGateHasABlockingFinding(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(*StepContext) (*StepOutcome, error) {
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      `{"findings":[{"id":"f-1","severity":"warning","description":"needs a decision","action":"ask-user"}],"summary":"1 issue"}`,
			}, nil
		},
	}
	exec := NewExecutor(database, p, &config.Config{}, newFakeSessionAgent(), []Step{step}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	err := exec.Respond(types.StepReview, types.ActionRereview, nil)
	var refusal *RespondRefusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Message, "rereview applies only") {
		t.Fatalf("Respond(rereview) error = %v, want a refusal naming the coverage-only scope", err)
	}
	// The refusal left the gate parked: a verdict still releases it.
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("the refused rereview must leave the gate parked for approval: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not finish after approval")
	}
}

func TestExecutor_RereviewIsRefusedForAnyStepButReview(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := newApprovalStep(types.StepDocument, `{"findings":[],"summary":"nothing"}`)
	exec := NewExecutor(database, p, &config.Config{}, newFakeSessionAgent(), []Step{step}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()
	waitForStepStatus(t, database, run.ID, types.StepDocument, types.StepStatusAwaitingApproval)

	err := exec.Respond(types.StepDocument, types.ActionRereview, nil)
	var refusal *RespondRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("Respond(rereview) on a non-review gate error = %v, want a refusal", err)
	}
	if err := exec.Respond(types.StepDocument, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve after the refused rereview: %v", err)
	}
	<-done
}

// A park lasts long enough for the daemon to restart under it; the recovered
// gate must take the same response and hand the step the same state as the
// live one, including a fix_review gate's context.
func TestExecutor_RecoveredCoverageGateTakesARereview(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status types.StepStatus
		fixing bool
	}{
		{"awaiting_approval", types.StepStatusAwaitingApproval, false},
		{"fix_review", types.StepStatusFixReview, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			stepResult, err := database.InsertStepResult(run.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.StartStep(stepResult.ID); err != nil {
				t.Fatal(err)
			}
			findings := coveragePark
			if err := database.SetStepFindings(stepResult.ID, findings); err != nil {
				t.Fatal(err)
			}
			if _, err := database.InsertReviewStepRound(stepResult.ID, 1, "initial", &findings, nil, "", 25); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateStepStatusWithDuration(stepResult.ID, tc.status, 25); err != nil {
				t.Fatal(err)
			}
			if err := database.SetRunAwaitingAgent(run.ID); err != nil {
				t.Fatal(err)
			}
			run, err = database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}

			var sawCoverage string
			var sawFixing, sawSkipFix, sawAnswering bool
			ran := make(chan struct{})
			step := &adaptiveCallStep{
				name: types.StepReview,
				fn: func(sctx *StepContext) (*StepOutcome, error) {
					sawCoverage = sctx.CoverageFindings
					sawFixing = sctx.Fixing
					sawSkipFix = sctx.SkipFixExecution
					sawAnswering = sctx.FinalizingAnswers
					close(ran)
					return coverageCompleteOutcome(), nil
				},
			}
			exec := NewExecutor(database, p, &config.Config{}, newFakeSessionAgent(), []Step{step}, nil)

			done := make(chan error, 1)
			go func() { done <- exec.Resume(context.Background(), run, repo, t.TempDir()) }()

			deadline := time.Now().Add(5 * time.Second)
			for {
				err := exec.Respond(types.StepReview, types.ActionRereview, nil)
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("recovered coverage gate never accepted a rereview: %v", err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			select {
			case <-ran:
			case <-time.After(5 * time.Second):
				t.Fatal("recovered rereview never reached the review step")
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("resume: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("recovered executor timed out")
			}

			if !strings.Contains(sawCoverage, `"reviewed_paths":["a.go"]`) {
				t.Fatalf("recovered rereview did not hand the step the parked record: %q", sawCoverage)
			}
			if sawAnswering {
				t.Fatal("a rereview must not read as an answer round")
			}
			if sawFixing != tc.fixing || sawSkipFix != tc.fixing {
				t.Fatalf("fixing=%v skipFix=%v, want both %v: a fix_review gate keeps its context without re-running the fixer", sawFixing, sawSkipFix, tc.fixing)
			}
			rounds, err := database.GetRoundsByStep(stepResult.ID)
			if err != nil {
				t.Fatal(err)
			}
			last := rounds[len(rounds)-1]
			if last.Trigger != "rereview" || last.IsFixRound() {
				t.Fatalf("recovered round trigger = %q (fix round: %v), want a non-fix \"rereview\"", last.Trigger, last.IsFixRound())
			}
		})
	}
}
