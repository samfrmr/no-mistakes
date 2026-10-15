package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

// These tests cover the operator's rereview of a review that parked on
// incomplete coverage (types.ActionRereview). A review whose coverage record
// is still incomplete after the focused completion pass parks with ZERO
// findings: `fix` has nothing to select and approve waives the unread files,
// which left no supported way to obtain the rest of the review. The rereview
// resumes from the parked record, spends one focused pass on exactly the
// unverified files, and still parks on anything left uncovered.

// parkOnIncompleteCoverage runs a review whose first turn covers firstTurn and
// whose focused pass fails, so the step parks on an incomplete record, and
// returns the parked findings payload an operator's rereview would extend.
func parkOnIncompleteCoverage(t *testing.T, firstTurn []string) string {
	t.Helper()
	dir, baseSHA, headSHA := twoFileRepo(t)
	ag := &mockAgent{name: "coverage-park"}
	ag.runFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if strings.Contains(opts.Prompt, coverageCompletionMarker) {
			return &agent.Result{Text: "prose only, no structured findings"}, nil
		}
		return &agent.Result{Output: json.RawMessage(coverageFindingJSON(firstTurn))}, nil
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("parking Execute() error = %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("setup: an incomplete record with a failed focused pass must park")
	}
	if outcome.Findings == "" {
		t.Fatal("setup: the parked round must carry the findings payload a rereview extends")
	}
	return outcome.Findings
}

func TestReviewStep_RereviewExtendsTheParkedRecordWithOneFocusedPass(t *testing.T) {
	t.Parallel()
	parked := parkOnIncompleteCoverage(t, []string{"feature.txt"})

	dir, baseSHA, headSHA := twoFileRepo(t)
	ag := newCoverageScriptedAgent(t, []coverageScript{
		// Exactly one turn, and it is the focused one: the parked round already
		// read feature.txt, so a rereview must not repeat the whole review.
		{onCompletion: true, output: coverageFindingJSON([]string{"feature2.txt"})},
	})
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.CoverageFindings = parked
	var logs []string
	sctx.Log = func(msg string) { logs = append(logs, msg) }

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("rereview Execute() error = %v", err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent invocations = %d, want 1 focused pass", len(ag.calls))
	}
	if outcome.NeedsApproval {
		t.Fatalf("a rereview that completed the record must certify the head; logs:\n%s", strings.Join(logs, "\n"))
	}
	if got := outcome.ReviewedPaths; len(got) != 2 {
		t.Fatalf("merged ReviewedPaths = %v, want the parked file plus the one the rereview covered", got)
	}
	prompt := ag.calls[0].Prompt
	if !strings.Contains(prompt, "  - feature2.txt\n") || strings.Contains(prompt, "  - feature.txt\n") {
		t.Fatalf("the focused pass must name exactly the unverified file:\n%s", prompt)
	}
	if ag.calls[0].Purpose != "review-coverage" {
		t.Fatalf("purpose = %q, want review-coverage so the turn is auditable", ag.calls[0].Purpose)
	}
}

// The barmkin shape: a reviewer that exits after one message and records no
// coverage at all leaves every file unverified. The rereview covers the whole
// reviewable set in the focused pass instead of re-running the review blind.
func TestReviewStep_RereviewCoversAReviewThatRecordedNoCoverage(t *testing.T) {
	t.Parallel()
	parked := parkOnIncompleteCoverage(t, nil)

	dir, baseSHA, headSHA := twoFileRepo(t)
	ag := newCoverageScriptedAgent(t, []coverageScript{
		{onCompletion: true, output: coverageFindingJSON([]string{"feature.txt", "feature2.txt"})},
	})
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.CoverageFindings = parked

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("rereview Execute() error = %v", err)
	}
	if outcome.NeedsApproval || len(outcome.ReviewedPaths) != 2 {
		t.Fatalf("NeedsApproval=%v ReviewedPaths=%v, want a complete record from the focused pass", outcome.NeedsApproval, outcome.ReviewedPaths)
	}
}

// A rereview is never an approval: whatever its focused pass leaves uncovered
// parks again with the remainder named, so the operator can ask once more.
func TestReviewStep_RereviewStillParksOnAnythingLeftUncovered(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		script []coverageScript
	}{
		{"focused pass covers nothing new", []coverageScript{{onCompletion: true, output: coverageFindingJSON([]string{"feature.txt"})}}},
		{"focused pass returns an empty record", []coverageScript{{onCompletion: true, output: coverageFindingJSON([]string{})}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parked := parkOnIncompleteCoverage(t, []string{"feature.txt"})

			dir, baseSHA, headSHA := twoFileRepo(t)
			ag := newCoverageScriptedAgent(t, tc.script)
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.CoverageFindings = parked
			var logs []string
			sctx.Log = func(msg string) { logs = append(logs, msg) }

			outcome, err := (&ReviewStep{}).Execute(sctx)
			if err != nil {
				t.Fatalf("rereview Execute() error = %v", err)
			}
			if !outcome.NeedsApproval {
				t.Fatal("a rereview that left a file unverified must park again")
			}
			if joined := strings.Join(logs, "\n"); !strings.Contains(joined, "1 reviewable file(s) unverified: feature2.txt") {
				t.Fatalf("the re-park must name the remaining file; logs:\n%s", joined)
			}
		})
	}
}

// A payload the rereview cannot extend without laundering it falls back to a
// full fresh review, which is always safe: an unreadable record, one carrying
// a blocking finding, and one with an entry that is not a path at all (that
// entry fails the strict check whatever a later turn covers).
func TestReviewStep_RereviewFallsBackToAFreshReviewForAnUnusableRecord(t *testing.T) {
	t.Parallel()
	blocking := `{"findings":[{"id":"r1","severity":"warning","file":"feature.txt","line":1,"description":"d","action":"ask-user"}],"summary":"s","reviewed_paths":["feature.txt"]}`
	cases := map[string]string{
		"unreadable":    `not json`,
		"blocking item": blocking,
		"invalid entry": `{"findings":[],"summary":"s","reviewed_paths":["feature.txt","  "]}`,
	}
	for name, parked := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := twoFileRepo(t)
			ag := newCoverageScriptedAgent(t, []coverageScript{
				{onCompletion: false, output: coverageFindingJSON([]string{"feature.txt", "feature2.txt"})},
			})
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.CoverageFindings = parked
			var logs []string
			sctx.Log = func(msg string) { logs = append(logs, msg) }

			outcome, err := (&ReviewStep{}).Execute(sctx)
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if len(ag.calls) != 1 || strings.Contains(ag.calls[0].Prompt, coverageCompletionMarker) {
				t.Fatalf("an unusable parked record must start a full review, calls = %d", len(ag.calls))
			}
			if outcome.NeedsApproval {
				t.Fatalf("the fresh review covered everything; logs:\n%s", strings.Join(logs, "\n"))
			}
			if joined := strings.Join(logs, "\n"); !strings.Contains(joined, "running a fresh review") {
				t.Fatalf("the fallback must be visible in the step log:\n%s", joined)
			}
		})
	}
}
