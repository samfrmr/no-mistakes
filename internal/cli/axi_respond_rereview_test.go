package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

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
