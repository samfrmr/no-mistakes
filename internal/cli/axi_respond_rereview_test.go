package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A review gate parked on incomplete coverage has no finding, so `fix` is
// refused. The refusal and the gate's own help must lead to the rereview
// instead of leaving approval - which waives the unread files - as the only
// visible response.
func TestAxiRespond_FixRefusalPointsAtTheRereview(t *testing.T) {
	responded := atomic.Bool{}
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		respond: func(context.Context, json.RawMessage) (interface{}, error) {
			responded.Store(true)
			return &ipc.RespondResult{OK: true}, nil
		},
	})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		return fx.awaiting(), nil
	})

	out, err := executeCmd("axi", "respond", "--action", "fix", "--wait", "3s")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("error = %v, want usage exit 2\n%s", err, out)
	}
	if !strings.Contains(out, "--action rereview") {
		t.Fatalf("the refusal does not name the rereview:\n%s", out)
	}
	if responded.Load() {
		t.Fatal("a refused fix still reached the daemon")
	}
}

func TestAxiRespond_RereviewIsForwardedWithoutAFindingSelection(t *testing.T) {
	var got ipc.RespondParams
	var responded atomic.Bool
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		respond: func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			if err := json.Unmarshal(raw, &got); err != nil {
				return nil, err
			}
			responded.Store(true)
			return &ipc.RespondResult{OK: true}, nil
		},
	})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		if responded.Load() {
			return fx.completed(), nil
		}
		return fx.awaiting(), nil
	})

	out, err := executeCmd("axi", "respond", "--action", "rereview", "--wait", "3s")
	if err != nil {
		t.Fatalf("rereview: %v\n%s", err, out)
	}
	if got.Action != types.ActionRereview {
		t.Fatalf("forwarded action = %q, want rereview", got.Action)
	}
	if len(got.FindingIDs) != 0 || len(got.IgnoreFindingIDs) != 0 || len(got.AddedFindings) != 0 {
		t.Fatalf("a rereview carries no finding selection: %+v", got)
	}
}

// --instructions and --add-finding belong to a fix; on a rereview they would be
// silently dropped, so they are refused before the daemon sees the response.
func TestAxiRespond_RereviewRefusesFixOnlyFlags(t *testing.T) {
	for _, flags := range [][]string{
		{"--instructions", "look harder"},
		{"--add-finding", `{"description":"x","action":"auto-fix"}`},
		{"--findings", "R1"},
		{"--ignore", "R1"},
	} {
		t.Run(flags[0], func(t *testing.T) {
			responded := atomic.Bool{}
			fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
				respond: func(context.Context, json.RawMessage) (interface{}, error) {
					responded.Store(true)
					return &ipc.RespondResult{OK: true}, nil
				},
			})
			fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
				return fx.awaiting(), nil
			})
			args := append([]string{"axi", "respond", "--action", "rereview", "--wait", "3s"}, flags...)
			out, err := executeCmd(args...)
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != 2 {
				t.Fatalf("error = %v, want usage exit 2\n%s", err, out)
			}
			if responded.Load() {
				t.Fatal("a rereview carrying fix-only flags still reached the daemon")
			}
		})
	}
}

// The gate help leads with the rereview only for a review gate whose findings
// leave coverage as the sole reason it is parked.
func TestGateHelpOffersTheRereviewOnlyOnACoverageGate(t *testing.T) {
	help := func(gate stepView) string {
		var b strings.Builder
		for _, f := range gateFieldsForRun(gate, "") {
			if lines, ok := f.Value.([]string); ok && f.Key == "help" {
				b.WriteString(strings.Join(lines, "\n"))
			}
		}
		return b.String()
	}
	clean := `{"findings":[],"summary":"clean","reviewed_paths":["a.go"]}`
	blocking := `{"findings":[{"id":"r1","severity":"warning","description":"d","action":"ask-user"}],"summary":"s"}`

	if got := help(stepView{Name: "review", Status: "awaiting_approval", FindingsJSON: clean}); !strings.Contains(got, "--action rereview") {
		t.Fatalf("a coverage gate's help does not offer the rereview:\n%s", got)
	}
	if got := help(stepView{Name: "review", Status: "awaiting_approval", FindingsJSON: blocking}); strings.Contains(got, "rereview") {
		t.Fatalf("a gate with a blocking finding must not offer the rereview:\n%s", got)
	}
	if got := help(stepView{Name: "document", Status: "awaiting_approval", FindingsJSON: clean}); strings.Contains(got, "rereview") {
		t.Fatalf("a non-review gate must not offer the rereview:\n%s", got)
	}
}

// --yes is standing consent to fix findings, not to waive coverage. A review
// parked on incomplete coverage carries no finding, which gateResolution used
// to read as "nothing to decide" and approve - certifying files nobody read.
// --yes now asks for the rest of the review exactly once per step, never
// approves, and leaves a gate that is still incomplete awaiting a human.
func TestDriveRun_YesNeverApprovesAnIncompleteReview(t *testing.T) {
	for _, status := range []types.StepStatus{types.StepStatusAwaitingApproval, types.StepStatusFixReview} {
		t.Run(string(status), func(t *testing.T) {
			socketPath := filepath.Join(makeSocketSafeTempDir(t), "coverage.sock")
			srv := ipc.NewServer()
			var mu sync.Mutex
			var actions []types.ApprovalAction
			srv.Handle(ipc.MethodRespond, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
				var params ipc.RespondParams
				if err := json.Unmarshal(raw, &params); err != nil {
					return nil, err
				}
				mu.Lock()
				actions = append(actions, params.Action)
				mu.Unlock()
				return &ipc.RespondResult{OK: true}, nil
			})
			done := make(chan error, 1)
			go func() { done <- srv.Serve(socketPath) }()
			t.Cleanup(func() {
				srv.Close()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("IPC server did not stop")
				}
			})
			var client *ipc.Client
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				var err error
				if client, err = ipc.Dial(socketPath); err == nil {
					break
				}
			}
			if client == nil {
				t.Fatal("IPC server did not become ready")
			}
			defer client.Close()

			findings := `{"findings":[],"summary":"clean","reviewed_paths":["a.go"]}`
			parked := func() *ipc.RunInfo {
				return &ipc.RunInfo{ID: "run-1", Status: types.RunRunning, Steps: []ipc.StepResultInfo{{StepName: types.StepReview, Status: status, FindingsJSON: &findings}}}
			}
			running := &ipc.RunInfo{ID: "run-1", Status: types.RunRunning, Steps: []ipc.StepResultInfo{{StepName: types.StepReview, Status: types.StepStatusRunning}}}
			reparked := parked()
			source := &scriptedRunStateSource{
				subscriptions: []scriptedSubscription{{events: make(chan ipc.Event)}},
				runs:          []*ipc.RunInfo{parked(), running, reparked},
			}
			reconciler := newRunReconciler(source, "run-1")
			reconciler.heartbeatInterval = 20 * time.Millisecond
			defer reconciler.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var progress bytes.Buffer

			run, ciReady, err := driveRunWithReconciler(ctx, &progress, client, reconciler, "run-1", true)
			if err != nil || run != reparked || ciReady {
				t.Fatalf("--yes did not stop at the still-incomplete review: run=%+v ciReady=%v err=%v", run, ciReady, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(actions) != 1 || actions[0] != types.ActionRereview {
				t.Fatalf("--yes responses = %v, want exactly one rereview and never an approval", actions)
			}
			if !strings.Contains(progress.String(), "never approves unverified files") {
				t.Fatalf("the operator is not told why --yes stopped:\n%s", progress.String())
			}
		})
	}
}
