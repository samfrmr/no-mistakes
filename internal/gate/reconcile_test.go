package gate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReconcileStaleBranchArchivesPatchEquivalentHeadBeforeNonForcePush(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	work := initReconcileRepo(t)
	base := reconcileGit(t, work, "rev-parse", "HEAD")

	writeReconcileFile(t, work, "feature.txt", "same change\n")
	reconcileGit(t, work, "add", "feature.txt")
	reconcileGit(t, work, "commit", "-m", "private rewrite")
	privateHead := reconcileGit(t, work, "rev-parse", "HEAD")

	reconcileGit(t, work, "reset", "--hard", base)
	writeReconcileFile(t, work, "base.txt", "base advanced\n")
	reconcileGit(t, work, "add", "base.txt")
	reconcileGit(t, work, "commit", "-m", "advance base")
	writeReconcileFile(t, work, "feature.txt", "same change\n")
	reconcileGit(t, work, "add", "feature.txt")
	reconcileGit(t, work, "commit", "-m", "rebased private rewrite")
	liveHead := reconcileGit(t, work, "rev-parse", "HEAD")

	gateDir := filepath.Join(t.TempDir(), "gate.git")
	reconcileGit(t, "", "init", "--bare", gateDir)
	reconcileGit(t, gateDir, "fetch", work, privateHead+":refs/heads/feature/reconcile")

	before, pushErr := exec.Command("git", "-C", work, "push", gateDir, liveHead+":refs/heads/feature/reconcile").CombinedOutput()
	if pushErr == nil || !strings.Contains(string(before), "non-fast-forward") {
		t.Fatalf("fixture must reproduce ordinary push rejection: %s, err=%v", before, pushErr)
	}
	t.Logf("Before reconciliation, ordinary push: %s", before)

	result, err := ReconcileStaleBranch(ctx, gateDir, work, "feature/reconcile", liveHead, "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reconciled || result.PreviousHead != privateHead || result.ArchivedTag == "" {
		t.Fatalf("reconciliation result = %+v", result)
	}
	if got := reconcileGit(t, gateDir, "rev-parse", result.ArchivedTag+"^{commit}"); got != privateHead {
		t.Fatalf("archive tag points at %s, want %s", got, privateHead)
	}
	if out, err := exec.Command("git", "--git-dir="+gateDir, "rev-parse", "--verify", "refs/heads/feature/reconcile").CombinedOutput(); err == nil {
		t.Fatalf("stale branch ref still exists: %s", out)
	}

	// The live head now enters through an ordinary new-branch push. No force or
	// force-with-lease is used by the supported handoff.
	t.Logf("After reconciliation, ordinary push: %s", reconcileGit(t, work, "push", gateDir, liveHead+":refs/heads/feature/reconcile"))
	t.Logf("Persisted refs: %s", reconcileGit(t, gateDir, "for-each-ref", "--format=%(refname) %(objectname) %(symref)"))
	if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature/reconcile"); got != liveHead {
		t.Fatalf("non-force push reached %s, want %s", got, liveHead)
	}
}

func TestReconcileStaleBranchLeavesContainedAncestorForNonForcePush(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	work := initReconcileRepo(t)

	writeReconcileFile(t, work, "feature.txt", "private content\n")
	reconcileGit(t, work, "add", "feature.txt")
	reconcileGit(t, work, "commit", "-m", "private head")
	privateHead := reconcileGit(t, work, "rev-parse", "HEAD")

	writeReconcileFile(t, work, "live.txt", "later live work\n")
	reconcileGit(t, work, "add", "live.txt")
	reconcileGit(t, work, "commit", "-m", "live descendant")
	liveHead := reconcileGit(t, work, "rev-parse", "HEAD")

	gateDir := filepath.Join(t.TempDir(), "gate.git")
	reconcileGit(t, "", "init", "--bare", gateDir)
	reconcileGit(t, gateDir, "fetch", work, privateHead+":refs/heads/feature/reconcile")

	result, err := ReconcileStaleBranch(ctx, gateDir, work, "feature/reconcile", liveHead, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Reconciled {
		t.Fatalf("ancestor requires no destructive reconciliation: %+v", result)
	}
	if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature/reconcile"); got != privateHead {
		t.Fatalf("ancestor branch moved during inspection to %s, want %s", got, privateHead)
	}
	if tags := reconcileGit(t, gateDir, "tag", "--list", "no-mistakes-abandoned/*"); tags != "" {
		t.Fatalf("ancestor inspection unexpectedly archived a live branch: %q", tags)
	}

	reconcileGit(t, work, "push", gateDir, liveHead+":refs/heads/feature/reconcile")
	if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature/reconcile"); got != liveHead {
		t.Fatalf("non-force fast-forward reached %s, want %s", got, liveHead)
	}
}

func TestReconcileStaleBranchRefusesAndNamesUniquePrivateCommits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	work := initReconcileRepo(t)
	base := reconcileGit(t, work, "rev-parse", "HEAD")

	writeReconcileFile(t, work, "private.txt", "unique trailer trim\n")
	reconcileGit(t, work, "add", "private.txt")
	reconcileGit(t, work, "commit", "-m", "private-only trailer trim")
	firstPrivateHead := reconcileGit(t, work, "rev-parse", "HEAD")
	writeReconcileFile(t, work, "second-private.txt", "another unique change\n")
	reconcileGit(t, work, "add", "second-private.txt")
	reconcileGit(t, work, "commit", "-m", "second private-only change")
	privateHead := reconcileGit(t, work, "rev-parse", "HEAD")

	reconcileGit(t, work, "reset", "--hard", base)
	writeReconcileFile(t, work, "live.txt", "different live work\n")
	reconcileGit(t, work, "add", "live.txt")
	reconcileGit(t, work, "commit", "-m", "live branch work")
	liveHead := reconcileGit(t, work, "rev-parse", "HEAD")

	gateDir := filepath.Join(t.TempDir(), "gate.git")
	reconcileGit(t, "", "init", "--bare", gateDir)
	reconcileGit(t, gateDir, "fetch", work, privateHead+":refs/heads/feature/reconcile")

	result, err := ReconcileStaleBranch(ctx, gateDir, work, "feature/reconcile", liveHead, "")
	if err == nil {
		t.Fatal("unique private commit was reconciled instead of refused")
	}
	if result.Reconciled {
		t.Fatalf("unique private commit reported reconciliation: %+v", result)
	}
	t.Logf("Publication refusal: %v", err)
	t.Logf("Preserved private branch: %s", reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature/reconcile"))
	for _, want := range []string{firstPrivateHead, "private-only trailer trim", privateHead, "second private-only change"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal did not name every at-risk commit; missing %q in: %v", want, err)
		}
	}
	if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature/reconcile"); got != privateHead {
		t.Fatalf("refusal moved private branch to %s, want %s", got, privateHead)
	}
	if tags := reconcileGit(t, gateDir, "tag", "--list", "no-mistakes-abandoned/*"); tags != "" {
		t.Fatalf("refusal created an archive tag despite retaining the branch: %q", tags)
	}
}

func TestReconcileStaleBranchArchivesRunOwnedHeadWithoutPatchEquivalence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	work := initReconcileRepo(t)
	base := reconcileGit(t, work, "rev-parse", "HEAD")

	// The head this run was launched from.
	writeReconcileFile(t, work, "feature.txt", "submitted resolution\n")
	reconcileGit(t, work, "add", "feature.txt")
	reconcileGit(t, work, "commit", "-m", "submitted work")
	submittedHead := reconcileGit(t, work, "rev-parse", "HEAD")

	// The rebased lineage resolves a conflict, so its per-file patch differs
	// from the submitted commit even though the run owns both.
	reconcileGit(t, work, "reset", "--hard", base)
	writeReconcileFile(t, work, "feature.txt", "upstream neighbour\n")
	reconcileGit(t, work, "add", "feature.txt")
	reconcileGit(t, work, "commit", "-m", "advance base")
	writeReconcileFile(t, work, "feature.txt", "upstream neighbour\nsubmitted resolution\n")
	reconcileGit(t, work, "add", "feature.txt")
	reconcileGit(t, work, "commit", "-m", "rebased with resolved conflict")
	liveHead := reconcileGit(t, work, "rev-parse", "HEAD")

	gateDir := filepath.Join(t.TempDir(), "gate.git")
	reconcileGit(t, "", "init", "--bare", gateDir)
	reconcileGit(t, gateDir, "fetch", work, submittedHead+":refs/heads/feature/reconcile")

	// Without run ownership the changed patch is genuinely unproven.
	if _, err := ReconcileStaleBranch(ctx, gateDir, work, "feature/reconcile", liveHead, ""); err == nil {
		t.Fatal("changed patch was reconciled without proof of ownership")
	}

	result, err := ReconcileStaleBranch(ctx, gateDir, work, "feature/reconcile", liveHead, submittedHead)
	if err != nil {
		t.Fatalf("run-owned submitted head was refused: %v", err)
	}
	if !result.Reconciled || result.PreviousHead != submittedHead {
		t.Fatalf("reconciliation result = %+v", result)
	}
	if got := reconcileGit(t, gateDir, "rev-parse", result.ArchivedTag+"^{commit}"); got != submittedHead {
		t.Fatalf("run-owned head was removed without an archive: %s", got)
	}
	if !ArchivedHeadRecorded(ctx, gateDir, "feature/reconcile", submittedHead) {
		t.Fatal("archived run-owned head is not recorded as archived")
	}

	reconcileGit(t, work, "push", gateDir, liveHead+":refs/heads/feature/reconcile")
	if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature/reconcile"); got != liveHead {
		t.Fatalf("non-force push reached %s, want %s", got, liveHead)
	}
}

func TestPlanStaleBranchReconciliationMutatesNothingAndApplyRefusesMovedHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	work := initReconcileRepo(t)
	base := reconcileGit(t, work, "rev-parse", "HEAD")

	writeReconcileFile(t, work, "feature.txt", "same change\n")
	reconcileGit(t, work, "add", "feature.txt")
	reconcileGit(t, work, "commit", "-m", "private rewrite")
	privateHead := reconcileGit(t, work, "rev-parse", "HEAD")

	reconcileGit(t, work, "reset", "--hard", base)
	writeReconcileFile(t, work, "base.txt", "base advanced\n")
	reconcileGit(t, work, "add", "base.txt")
	reconcileGit(t, work, "commit", "-m", "advance base")
	writeReconcileFile(t, work, "feature.txt", "same change\n")
	reconcileGit(t, work, "add", "feature.txt")
	reconcileGit(t, work, "commit", "-m", "rebased private rewrite")
	liveHead := reconcileGit(t, work, "rev-parse", "HEAD")

	writeReconcileFile(t, work, "intervening.txt", "arrived after planning\n")
	reconcileGit(t, work, "add", "intervening.txt")
	reconcileGit(t, work, "commit", "-m", "intervening private commit")
	interveningHead := reconcileGit(t, work, "rev-parse", "HEAD")

	gateDir := filepath.Join(t.TempDir(), "gate.git")
	reconcileGit(t, "", "init", "--bare", gateDir)
	reconcileGit(t, gateDir, "fetch", work, privateHead+":refs/heads/feature/reconcile")

	plan, err := PlanStaleBranchReconciliation(ctx, gateDir, work, "feature/reconcile", liveHead, "")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Reconcile || plan.PreviousHead != privateHead {
		t.Fatalf("plan = %+v", plan)
	}
	// Planning alone must leave the private mirror exactly as it found it.
	if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature/reconcile"); got != privateHead {
		t.Fatalf("planning moved the private branch to %s", got)
	}
	if tags := reconcileGit(t, gateDir, "tag", "--list", "no-mistakes-abandoned/*"); tags != "" {
		t.Fatalf("planning archived a live branch: %q", tags)
	}

	reconcileGit(t, gateDir, "fetch", work, "+"+interveningHead+":refs/heads/feature/reconcile")
	if _, err := ApplyStaleBranchReconciliation(ctx, gateDir, plan); err == nil {
		t.Fatal("apply deleted a private head that arrived after the proof")
	}
	if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature/reconcile"); got != interveningHead {
		t.Fatalf("refused apply still moved the branch to %s", got)
	}
}

func TestArchivedHeadRecordedRejectsUnarchivedClaims(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	work := initReconcileRepo(t)
	head := reconcileGit(t, work, "rev-parse", "HEAD")

	gateDir := filepath.Join(t.TempDir(), "gate.git")
	reconcileGit(t, "", "init", "--bare", gateDir)
	reconcileGit(t, gateDir, "fetch", work, head+":refs/heads/feature/claim")

	if ArchivedHeadRecorded(ctx, gateDir, "feature/claim", head) {
		t.Fatal("a head with no archive tag was reported as archived")
	}
	reconcileGit(t, gateDir, "update-ref", "refs/tags/no-mistakes-abandoned/feature/claim/"+head, head)
	if !ArchivedHeadRecorded(ctx, gateDir, "feature/claim", head) {
		t.Fatal("an archived head was not recognized")
	}
	if ArchivedHeadRecorded(ctx, gateDir, "other/branch", head) {
		t.Fatal("an archive tag for one branch answered for another")
	}
	if ArchivedHeadRecorded(ctx, gateDir, "feature/claim", "") {
		t.Fatal("an empty claim was accepted")
	}
}

// TestReconcileStaleBranchAcceptsRebasedHeadFurtherExtendedByReviewFix
// reproduces the private-mirror false refusal from issue-derived recovery
// reports: a branch whose two commits were cleanly rebased onto a moved
// default branch, then further extended by a review-fix commit that revises
// the very file the rebased commits introduced. Before contributionSurvives
// existed, the single whole-tree `git merge-tree` comparison used privateHead
// and liveHead's ancient common ancestor as its merge base, so the freshly
// introduced file looked like an unresolvable add/add conflict to Git even
// though every byte of the original commits is still present. This is the
// "proven ordinary sync path" scenario with one added twist: an additional
// live-only commit legitimately revises the same file the rebase carried
// over, which is exactly what a review-fix round after a rebase looks like.
func TestReconcileStaleBranchAcceptsRebasedHeadFurtherExtendedByReviewFix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	work := initReconcileRepo(t)
	base := reconcileGit(t, work, "rev-parse", "HEAD")

	writeReconcileFile(t, work, "signing.md", "signing key custody notes\n")
	reconcileGit(t, work, "add", "signing.md")
	reconcileGit(t, work, "commit", "-m", "add signing-key custody notes")

	writeReconcileFile(t, work, "AGENTS.md", "# AGENTS.md\n\n## Self-governance\nInitial note.\n")
	reconcileGit(t, work, "add", "AGENTS.md")
	reconcileGit(t, work, "commit", "-m", "add AGENTS.md self-governance section")
	gateHead := reconcileGit(t, work, "rev-parse", "HEAD")

	// The default branch advances with an unrelated file while this branch's
	// two commits are pending review.
	reconcileGit(t, work, "checkout", "--detach", base)
	writeReconcileFile(t, work, "unrelated.txt", "advance\n")
	reconcileGit(t, work, "add", "unrelated.txt")
	reconcileGit(t, work, "commit", "-m", "advance base")
	newBase := reconcileGit(t, work, "rev-parse", "HEAD")

	// A clean rebase of the two original commits onto the moved base: neither
	// touches unrelated.txt, so both patches replay verbatim.
	reconcileGit(t, work, "checkout", "-b", "rebased", gateHead)
	reconcileGit(t, work, "rebase", newBase)

	// A review-fix round further revises the file the rebase carried over -
	// an ordinary, legitimate continuation of the same lineage.
	writeReconcileFile(t, work, "AGENTS.md", "# AGENTS.md\n\n## Self-governance\nInitial note.\n\nMore detail added during review.\n")
	reconcileGit(t, work, "add", "AGENTS.md")
	reconcileGit(t, work, "commit", "-m", "review fix: expand self-governance section")
	liveHead := reconcileGit(t, work, "rev-parse", "HEAD")

	gateDir := filepath.Join(t.TempDir(), "gate.git")
	reconcileGit(t, "", "init", "--bare", gateDir)
	reconcileGit(t, gateDir, "fetch", work, gateHead+":refs/heads/feature")

	result, err := ReconcileStaleBranch(ctx, gateDir, work, "feature", liveHead, "")
	if err != nil || !result.Reconciled || result.PreviousHead != gateHead {
		t.Fatalf("rebased head further extended by review fix was refused: result=%+v err=%v", result, err)
	}
	reconcileGit(t, work, "push", gateDir, liveHead+":refs/heads/feature")
	if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature"); got != liveHead {
		t.Fatalf("non-force push reached %s, want %s", got, liveHead)
	}
}

// TestReconcileStaleBranchRefusesWhenRebasedReplayDropsPrivateContent is the
// counterexample: a "rebase" that drops one of the two original commits
// entirely (as a broken tool or a bad manual replay might) must still be
// refused, naming the commit whose content never made it into the live head.
func TestReconcileStaleBranchRefusesWhenRebasedReplayDropsPrivateContent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	work := initReconcileRepo(t)
	base := reconcileGit(t, work, "rev-parse", "HEAD")

	writeReconcileFile(t, work, "signing.md", "signing key custody notes\n")
	reconcileGit(t, work, "add", "signing.md")
	reconcileGit(t, work, "commit", "-m", "add signing-key custody notes")
	droppedHead := reconcileGit(t, work, "rev-parse", "HEAD")

	writeReconcileFile(t, work, "AGENTS.md", "# AGENTS.md\n\n## Self-governance\nInitial note.\n")
	reconcileGit(t, work, "add", "AGENTS.md")
	reconcileGit(t, work, "commit", "-m", "add AGENTS.md self-governance section")
	gateHead := reconcileGit(t, work, "rev-parse", "HEAD")

	// A "rebase" that only replays the second commit's patch onto the moved
	// base, silently losing the first commit's file entirely.
	reconcileGit(t, work, "checkout", "--detach", base)
	writeReconcileFile(t, work, "unrelated.txt", "advance\n")
	reconcileGit(t, work, "add", "unrelated.txt")
	reconcileGit(t, work, "commit", "-m", "advance base")
	writeReconcileFile(t, work, "AGENTS.md", "# AGENTS.md\n\n## Self-governance\nInitial note.\n")
	reconcileGit(t, work, "add", "AGENTS.md")
	reconcileGit(t, work, "commit", "-m", "add AGENTS.md self-governance section")
	liveHead := reconcileGit(t, work, "rev-parse", "HEAD")

	gateDir := filepath.Join(t.TempDir(), "gate.git")
	reconcileGit(t, "", "init", "--bare", gateDir)
	reconcileGit(t, gateDir, "fetch", work, gateHead+":refs/heads/feature")

	result, err := ReconcileStaleBranch(ctx, gateDir, work, "feature", liveHead, "")
	if err == nil || result.Reconciled {
		t.Fatalf("genuinely missing content was reconciled: result=%+v err=%v", result, err)
	}
	if !strings.Contains(err.Error(), droppedHead) {
		t.Fatalf("refusal did not name the commit whose content is missing: %v", err)
	}
	if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature"); got != gateHead {
		t.Fatalf("refusal moved the private branch: %s", got)
	}
}

// TestReconcileStaleBranchRefusesBinaryContentChangedAfterRebase guards the
// line-based survival check's fail-closed path: a binary file's diff carries
// no +/- text lines, so a naive "no lines removed" reading would pass
// vacuously no matter what actually changed. A live-side commit that
// overwrites the private commit's binary content after a clean rebase must
// still be refused.
func TestReconcileStaleBranchRefusesBinaryContentChangedAfterRebase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	work := initReconcileRepo(t)
	base := reconcileGit(t, work, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(work, "asset.bin"), []byte{0x00, 0x01, 0x02, 0x03}, 0o644); err != nil {
		t.Fatal(err)
	}
	reconcileGit(t, work, "add", "asset.bin")
	reconcileGit(t, work, "commit", "-m", "add binary asset")
	gateHead := reconcileGit(t, work, "rev-parse", "HEAD")

	reconcileGit(t, work, "checkout", "--detach", base)
	writeReconcileFile(t, work, "unrelated.txt", "advance\n")
	reconcileGit(t, work, "add", "unrelated.txt")
	reconcileGit(t, work, "commit", "-m", "advance base")
	newBase := reconcileGit(t, work, "rev-parse", "HEAD")

	reconcileGit(t, work, "checkout", "-b", "rebased", gateHead)
	reconcileGit(t, work, "rebase", newBase)

	// A live-only commit overwrites the binary content the rebase carried
	// over - genuinely different bytes, not an extension of them.
	if err := os.WriteFile(filepath.Join(work, "asset.bin"), []byte{0xff, 0xee, 0xdd}, 0o644); err != nil {
		t.Fatal(err)
	}
	reconcileGit(t, work, "add", "asset.bin")
	reconcileGit(t, work, "commit", "-m", "replace binary asset")
	liveHead := reconcileGit(t, work, "rev-parse", "HEAD")

	gateDir := filepath.Join(t.TempDir(), "gate.git")
	reconcileGit(t, "", "init", "--bare", gateDir)
	reconcileGit(t, gateDir, "fetch", work, gateHead+":refs/heads/feature")

	result, err := ReconcileStaleBranch(ctx, gateDir, work, "feature", liveHead, "")
	if err == nil || result.Reconciled {
		t.Fatalf("changed binary content was reconciled: result=%+v err=%v", result, err)
	}
	if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature"); got != gateHead {
		t.Fatalf("refusal moved the private branch: %s", got)
	}
}

func initReconcileRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	reconcileGit(t, "", "init", dir)
	reconcileGit(t, dir, "config", "user.name", "test")
	reconcileGit(t, dir, "config", "user.email", "test@example.com")
	writeReconcileFile(t, dir, "base.txt", "base\n")
	reconcileGit(t, dir, "add", "base.txt")
	reconcileGit(t, dir, "commit", "-m", "base")
	return dir
}

func writeReconcileFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func reconcileGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestReconcileStaleBranchIncludesPatchesHiddenByMergeSimplification(t *testing.T) {
	t.Parallel()
	work := initReconcileRepo(t)
	base := reconcileGit(t, work, "rev-parse", "HEAD")
	writeReconcileFile(t, work, "feature.txt", "final content\n")
	reconcileGit(t, work, "add", "-A")
	reconcileGit(t, work, "commit", "-m", "private change")
	privateHead := reconcileGit(t, work, "rev-parse", "HEAD")
	reconcileGit(t, work, "checkout", "--detach", base)
	writeReconcileFile(t, work, "feature.txt", "final content\n")
	reconcileGit(t, work, "add", "-A")
	reconcileGit(t, work, "commit", "-m", "equivalent side change")
	sideHead := reconcileGit(t, work, "rev-parse", "HEAD")
	reconcileGit(t, work, "checkout", "--detach", base)
	writeReconcileFile(t, work, "feature.txt", "intermediate content\n")
	reconcileGit(t, work, "add", "-A")
	reconcileGit(t, work, "commit", "-m", "different initial patch")
	writeReconcileFile(t, work, "feature.txt", "final content\n")
	reconcileGit(t, work, "add", "-A")
	reconcileGit(t, work, "commit", "-m", "reach same tree through different patches")
	reconcileGit(t, work, "merge", "--no-ff", "-s", "ours", sideHead, "-m", "merge side history")
	liveHead := reconcileGit(t, work, "rev-parse", "HEAD")
	gateDir := filepath.Join(t.TempDir(), "gate.git")
	reconcileGit(t, "", "init", "--bare", gateDir)
	reconcileGit(t, gateDir, "fetch", work, privateHead+":refs/heads/feature")
	result, err := ReconcileStaleBranch(context.Background(), gateDir, work, "feature", liveHead, "")
	if err != nil || !result.Reconciled {
		t.Fatalf("merge-contained patch was refused: result=%+v err=%v", result, err)
	}
	if got := reconcileGit(t, gateDir, "rev-parse", result.ArchivedTag); got != privateHead {
		t.Fatalf("archive = %s, want %s", got, privateHead)
	}
	reconcileGit(t, work, "push", gateDir, liveHead+":refs/heads/feature")
	if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature"); got != liveHead {
		t.Fatalf("ordinary push reached %s, want %s", got, liveHead)
	}
}

func TestRestoreReconciledBranchPreservesConcurrentRefAndRequiresArchive(t *testing.T) {
	for _, state := range []string{"absent", "concurrent", "missing_archive"} {
		t.Run(state, func(t *testing.T) {
			work := initReconcileRepo(t)
			base := reconcileGit(t, work, "rev-parse", "HEAD")
			reconcileGit(t, work, "commit", "--allow-empty", "-m", "private")
			privateHead := reconcileGit(t, work, "rev-parse", "HEAD")
			reconcileGit(t, work, "checkout", "--detach", base)
			reconcileGit(t, work, "commit", "--allow-empty", "-m", "live")
			liveHead := reconcileGit(t, work, "rev-parse", "HEAD")
			gateDir := filepath.Join(t.TempDir(), "gate.git")
			reconcileGit(t, "", "init", "--bare", gateDir)
			reconcileGit(t, gateDir, "fetch", work, privateHead+":refs/heads/feature")
			result, err := ReconcileStaleBranch(context.Background(), gateDir, work, "feature", liveHead, "")
			if err != nil || !result.Reconciled {
				t.Fatalf("reconciliation = %+v, err = %v", result, err)
			}
			if state == "concurrent" {
				reconcileGit(t, gateDir, "update-ref", "refs/heads/feature", liveHead)
			}
			if state == "missing_archive" {
				reconcileGit(t, gateDir, "update-ref", "-d", result.ArchivedTag)
			}
			err = RestoreReconciledBranch(context.Background(), gateDir, "feature", result)
			if state == "missing_archive" {
				if err == nil {
					t.Fatal("restored without archive evidence")
				}
				if refs := reconcileGit(t, gateDir, "for-each-ref", "--format=%(refname)", "refs/heads/"); refs != "" {
					t.Fatalf("failed restoration created ref: %s", refs)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := privateHead
			if state == "concurrent" {
				want = liveHead
			}
			if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature"); got != want {
				t.Fatalf("restored ref = %s, want %s", got, want)
			}
		})
	}
}

func TestReconcileStaleBranchDecision41AExactSubmittedHeadOnly(t *testing.T) {
	for _, variant := range []string{"exact", "newer", "divergent", "abbreviated", "unknown"} {
		t.Run(variant, func(t *testing.T) {
			work := initReconcileRepo(t)
			base := reconcileGit(t, work, "rev-parse", "HEAD")
			writeReconcileFile(t, work, "submitted.txt", "submitted content\n")
			reconcileGit(t, work, "add", "-A")
			reconcileGit(t, work, "commit", "-m", "submitted work")
			submittedHead := reconcileGit(t, work, "rev-parse", "HEAD")
			privateHead := submittedHead
			ownedHead := submittedHead
			atRisk := []string{submittedHead}
			switch variant {
			case "newer", "divergent":
				if variant == "divergent" {
					reconcileGit(t, work, "checkout", "--detach", base)
					atRisk = nil
				}
				writeReconcileFile(t, work, "external.txt", "external content\n")
				reconcileGit(t, work, "add", "-A")
				reconcileGit(t, work, "commit", "-m", "external work")
				privateHead = reconcileGit(t, work, "rev-parse", "HEAD")
				atRisk = append(atRisk, privateHead)
			case "abbreviated":
				ownedHead = submittedHead[:12]
			case "unknown":
				ownedHead = ""
			}
			reconcileGit(t, work, "checkout", "--detach", base)
			writeReconcileFile(t, work, "live.txt", "reviewed rewrite\n")
			reconcileGit(t, work, "add", "-A")
			reconcileGit(t, work, "commit", "-m", "reviewed rewrite")
			liveHead := reconcileGit(t, work, "rev-parse", "HEAD")
			gateDir := filepath.Join(t.TempDir(), "gate.git")
			reconcileGit(t, "", "init", "--bare", gateDir)
			reconcileGit(t, gateDir, "fetch", work, privateHead+":refs/heads/feature")
			plan, err := PlanMirrorPublicationReconciliation(context.Background(), gateDir, work, "feature", liveHead, ownedHead)
			if variant != "exact" {
				if err == nil || plan.Reconcile {
					t.Fatalf("non-exact submitted head exempted: plan=%+v err=%v", plan, err)
				}
				for _, commit := range atRisk {
					if !strings.Contains(err.Error(), commit) {
						t.Fatalf("missing at-risk commit %s: %v", commit, err)
					}
				}
			} else {
				if err != nil || !plan.Reconcile {
					t.Fatalf("exact submitted-head exception refused: plan=%+v err=%v", plan, err)
				}
			}
			if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature"); got != privateHead {
				t.Fatalf("planning moved mirror to %s, want %s", got, privateHead)
			}
			if got := reconcileGit(t, gateDir, "tag", "--list", "no-mistakes-abandoned/*"); got != "" {
				t.Fatalf("planning archived head: %s", got)
			}
			if variant == "exact" {
				result, err := ApplyStaleBranchReconciliation(context.Background(), gateDir, plan)
				if err != nil || !result.Reconciled {
					t.Fatalf("apply = %+v, err = %v", result, err)
				}
				if got := reconcileGit(t, gateDir, "rev-parse", result.ArchivedTag); got != submittedHead {
					t.Fatalf("archive = %s, want exact submitted head %s", got, submittedHead)
				}
				reconcileGit(t, work, "push", gateDir, liveHead+":refs/heads/feature")
				if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature"); got != liveHead {
					t.Fatalf("ordinary push = %s, want %s", got, liveHead)
				}
			}
		})
	}
}

func TestReconcileStaleBranchRefusesPatchesDiscardedByOursMerge(t *testing.T) {
	for _, change := range []string{"add", "modify", "delete"} {
		t.Run(change, func(t *testing.T) {
			work := initReconcileRepo(t)
			if change != "add" {
				writeReconcileFile(t, work, "feature.txt", "original\n")
				reconcileGit(t, work, "add", "-A")
				reconcileGit(t, work, "commit", "-m", "original feature")
			}
			base := reconcileGit(t, work, "rev-parse", "HEAD")
			apply := func(message string) string {
				t.Helper()
				if change == "delete" {
					reconcileGit(t, work, "rm", "feature.txt")
				} else {
					writeReconcileFile(t, work, "feature.txt", "private change\n")
					reconcileGit(t, work, "add", "-A")
				}
				reconcileGit(t, work, "commit", "-m", message)
				return reconcileGit(t, work, "rev-parse", "HEAD")
			}
			privateHead := apply("private work")
			reconcileGit(t, work, "checkout", "--detach", base)
			sideHead := apply("equivalent side work")
			reconcileGit(t, work, "checkout", "--detach", base)
			writeReconcileFile(t, work, "live.txt", "live work\n")
			reconcileGit(t, work, "add", "-A")
			reconcileGit(t, work, "commit", "-m", "live work")
			beforeMergeTree := reconcileGit(t, work, "rev-parse", "HEAD^{tree}")
			reconcileGit(t, work, "merge", "--no-ff", "-s", "ours", sideHead, "-m", "discard side work")
			liveHead := reconcileGit(t, work, "rev-parse", "HEAD")
			if got := reconcileGit(t, work, "rev-parse", "HEAD^{tree}"); got != beforeMergeTree {
				t.Fatalf("ours merge unexpectedly retained the side change: %s", got)
			}
			gateDir := filepath.Join(t.TempDir(), "gate.git")
			reconcileGit(t, "", "init", "--bare", gateDir)
			reconcileGit(t, gateDir, "fetch", work, privateHead+":refs/heads/feature")
			result, err := ReconcileStaleBranch(context.Background(), gateDir, work, "feature", liveHead, "")
			if err == nil || result.Reconciled || !strings.Contains(err.Error(), privateHead) {
				t.Fatalf("discarded patch accepted or not named: result=%+v err=%v", result, err)
			}
			if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature"); got != privateHead {
				t.Fatalf("refusal moved private ref: %s", got)
			}
			if got := reconcileGit(t, gateDir, "tag", "--list", "no-mistakes-abandoned/*"); got != "" {
				t.Fatalf("discarded content was archived for deletion: %s", got)
			}
		})
	}
}
