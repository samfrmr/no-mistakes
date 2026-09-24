package gate

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// StaleBranchReconciliation reports a private gate branch that was archived
// and removed so the caller can submit the live head with an ordinary push.
type StaleBranchReconciliation struct {
	Reconciled   bool
	PreviousHead string
	ArchivedTag  string
}

// StaleBranchPlan is the verdict of a non-mutating stale-branch inspection.
// Planning checks containment or the exact submitted-head policy exception
// without touching any ref, so a caller can decide before it publishes anything;
// applying the plan is the only step that archives and removes the branch.
type StaleBranchPlan struct {
	PreserveDescendantOf string
	Reconcile            bool
	Branch               string
	BranchRef            string
	PreviousHead         string
	ArchiveTag           string
}

// ReconcileStaleBranch plans and immediately applies stale private gate branch
// reconciliation. It removes the branch only after Git proves the live head
// contains all of its content, or under the exact submitted-head exception
// described in docs/src/content/docs/concepts/gate-model.md.
func ReconcileStaleBranch(ctx context.Context, gateDir, workDir, branch, liveHead, runOwnedHead string) (StaleBranchReconciliation, error) {
	plan, err := PlanStaleBranchReconciliation(ctx, gateDir, workDir, branch, liveHead, runOwnedHead)
	if err != nil || !plan.Reconcile {
		return StaleBranchReconciliation{}, err
	}
	return ApplyStaleBranchReconciliation(ctx, gateDir, plan)
}

// PlanStaleBranchReconciliation inspects a private gate branch and reports
// whether it must be archived and removed before the live head can enter
// through an ordinary push. It mutates no ref: outside the submitted-head
// exception, an unproven private head is refused before publication.
//
// Rewritten histories require both stable per-file patch identities and final
// tree survival. runOwnedHead is a policy exception, not containment evidence:
// publication callers must supply only Run.SubmittedHeadSHA, and fresh
// submissions must leave it empty. The contract and rationale are owned by
// docs/src/content/docs/concepts/gate-model.md (Private mirror reconciliation).
func PlanStaleBranchReconciliation(ctx context.Context, gateDir, workDir, branch, liveHead, runOwnedHead string) (StaleBranchPlan, error) {
	return planStaleBranchReconciliation(ctx, gateDir, workDir, branch, liveHead, runOwnedHead, "", false)
}

// PlanMirrorPublicationReconciliation is PlanStaleBranchReconciliation for the
// publish-time caller (push.go), which additionally may supply
// recoveryExactHead: a second, independent policy exception from Decision
// 41-A. See planStaleBranchReconciliation's doc comment for its exact
// contract; push.go's recoveryMirrorExactHead is the only computer of it.
func PlanMirrorPublicationReconciliation(ctx context.Context, gateDir, workDir, branch, liveHead, runOwnedHead, recoveryExactHead string) (StaleBranchPlan, error) {
	return planStaleBranchReconciliation(ctx, gateDir, workDir, branch, liveHead, runOwnedHead, recoveryExactHead, true)
}

// planStaleBranchReconciliation supports two independent, additive policy
// exceptions to the content-survival scan, both compared against the STALE
// gate ref (never the live head): runOwnedHead is Decision 41-A (a run
// republishing exactly its own already-submitted head); recoveryExactHead is
// the fresh-review recovery exception (a run whose durably review-approved
// head is proven, by the caller, to be the exact preserved head of a prior
// terminal run whose own submitted head is the stale gate ref being
// replaced). Neither is containment evidence on its own - both are the
// caller's job to justify before calling; this function only compares refs.
func planStaleBranchReconciliation(ctx context.Context, gateDir, workDir, branch, liveHead, runOwnedHead, recoveryExactHead string, preserveDescendants bool) (StaleBranchPlan, error) {
	var plan StaleBranchPlan
	branch = strings.TrimSpace(branch)
	liveHead = strings.TrimSpace(liveHead)
	runOwnedHead = strings.TrimSpace(runOwnedHead)
	recoveryExactHead = strings.TrimSpace(recoveryExactHead)
	if branch == "" || liveHead == "" {
		return plan, fmt.Errorf("reconcile stale gate branch: branch and live head are required")
	}
	if err := git.ValidateBareRepository(ctx, gateDir); err != nil {
		return plan, fmt.Errorf("reconcile stale gate branch: %w", err)
	}
	if _, err := git.Run(ctx, workDir, "check-ref-format", "--branch", branch); err != nil {
		return plan, fmt.Errorf("reconcile stale gate branch %q: invalid branch name: %w", branch, err)
	}
	resolvedLive, err := git.Run(ctx, workDir, "rev-parse", "--verify", liveHead+"^{commit}")
	if err != nil || resolvedLive != liveHead {
		return plan, fmt.Errorf("reconcile stale gate branch %s: live head %s is not an exact commit", branch, liveHead)
	}
	workDir, err = filepath.Abs(workDir)
	if err != nil {
		return plan, fmt.Errorf("reconcile stale gate branch %s: resolve worktree path: %w", branch, err)
	}
	branchRef := "refs/heads/" + branch
	gateHead, exists, err := git.DirectRefTarget(ctx, gateDir, branchRef)
	if err != nil {
		return plan, fmt.Errorf("inspect private mirror ref %s: %w", branchRef, err)
	}
	if !exists {
		return plan, nil
	}
	archiveTag := "refs/tags/no-mistakes-abandoned/" + branch + "/" + gateHead
	archivedHead, archived, err := git.DirectRefTarget(ctx, gateDir, archiveTag)
	if err != nil {
		return plan, fmt.Errorf("inspect private mirror archive tag %s: %w", archiveTag, err)
	}
	if archived && archivedHead != gateHead {
		return plan, fmt.Errorf("private mirror archive tag %s already points at %s, not %s", archiveTag, archivedHead, gateHead)
	}
	if gateHead == liveHead {
		return plan, nil
	}
	if objectType, err := git.Run(ctx, gateDir, "cat-file", "-t", gateHead); err != nil || objectType != "commit" {
		return plan, fmt.Errorf("private mirror ref %s does not point at a commit", branchRef)
	}
	if err := git.FetchRemoteRef(ctx, gateDir, workDir, liveHead, liveHead); err != nil {
		return plan, fmt.Errorf("stage live head for private mirror reconciliation: %w", err)
	}

	// An ancestor needs no reconciliation: the caller's ordinary push is
	// already a fast-forward and preserves the private head by ancestry.
	if _, err := git.Run(ctx, gateDir, "merge-base", "--is-ancestor", gateHead, liveHead); err == nil {
		return plan, nil
	}
	if preserveDescendants {
		plan.PreserveDescendantOf = liveHead
		if _, err := git.Run(ctx, gateDir, "merge-base", "--is-ancestor", liveHead, gateHead); err == nil {
			return plan, nil
		}
	}
	if gateHead != runOwnedHead && gateHead != recoveryExactHead {
		atRiskCommits, err := privateCommitsAbsentFromLive(ctx, gateDir, liveHead, gateHead)
		if err != nil {
			return plan, fmt.Errorf("compare private mirror content for %s: %w", branchRef, err)
		}
		if len(atRiskCommits) > 0 {
			atRisk := make([]string, 0, len(atRiskCommits))
			for _, commit := range atRiskCommits {
				description, describeErr := git.Run(ctx, gateDir, "show", "-s", "--format=%H %s", commit)
				if describeErr != nil {
					return plan, fmt.Errorf("describe at-risk private mirror commit %s: %w", commit, describeErr)
				}
				atRisk = append(atRisk, description)
			}
			return plan, fmt.Errorf(
				"refusing to reconcile private mirror ref %s: %d at-risk commit(s) contain content absent from live head %s: %s",
				branchRef, len(atRisk), liveHead, strings.Join(atRisk, "; "),
			)
		}
	}

	return StaleBranchPlan{
		PreserveDescendantOf: plan.PreserveDescendantOf,
		Reconcile:            true,
		Branch:               branch,
		BranchRef:            branchRef,
		PreviousHead:         gateHead,
		ArchiveTag:           archiveTag,
	}, nil
}

// ApplyStaleBranchReconciliation archives the planned head and then removes the
// branch ref. It revalidates that the branch still points at the exact head the
// plan proved, so a private head that appeared after planning is never deleted.
func ApplyStaleBranchReconciliation(ctx context.Context, gateDir string, plan StaleBranchPlan) (StaleBranchReconciliation, error) {
	var result StaleBranchReconciliation
	if !plan.Reconcile {
		return result, nil
	}
	if plan.BranchRef == "" || plan.PreviousHead == "" || plan.ArchiveTag == "" {
		return result, fmt.Errorf("apply private mirror reconciliation: incomplete plan for %q", plan.Branch)
	}
	if err := git.ValidateBareRepository(ctx, gateDir); err != nil {
		return result, fmt.Errorf("apply private mirror reconciliation: %w", err)
	}
	currentHead, exists, err := git.DirectRefTarget(ctx, gateDir, plan.BranchRef)
	if err != nil {
		return result, fmt.Errorf("inspect private mirror ref %s: %w", plan.BranchRef, err)
	}
	if !exists {
		return result, nil
	}
	if currentHead != plan.PreviousHead {
		if plan.PreserveDescendantOf != "" {
			if _, err := git.Run(ctx, gateDir, "merge-base", "--is-ancestor", plan.PreserveDescendantOf, currentHead); err == nil {
				return result, nil
			}
		}
		return result, fmt.Errorf(
			"private mirror ref %s moved to %s after it was proven stale at %s",
			plan.BranchRef, currentHead, plan.PreviousHead,
		)
	}
	archivedHead, archived, err := git.DirectRefTarget(ctx, gateDir, plan.ArchiveTag)
	if err != nil {
		return result, fmt.Errorf("inspect private mirror archive tag %s: %w", plan.ArchiveTag, err)
	}
	if archived && archivedHead != plan.PreviousHead {
		return result, fmt.Errorf("private mirror archive tag %s already points at %s, not %s", plan.ArchiveTag, archivedHead, plan.PreviousHead)
	}
	if !archived {
		if _, err := git.Run(ctx, gateDir, "update-ref", "--no-deref", plan.ArchiveTag, plan.PreviousHead, strings.Repeat("0", len(plan.PreviousHead))); err != nil {
			return result, fmt.Errorf("archive stale private mirror head %s at %s: %w", plan.PreviousHead, plan.ArchiveTag, err)
		}
	}
	if _, err := git.Run(ctx, gateDir, "update-ref", "--no-deref", "-d", plan.BranchRef, plan.PreviousHead); err != nil {
		return result, fmt.Errorf("delete archived stale private mirror ref %s at %s: %w", plan.BranchRef, plan.PreviousHead, err)
	}
	return StaleBranchReconciliation{Reconciled: true, PreviousHead: plan.PreviousHead, ArchivedTag: plan.ArchiveTag}, nil
}

func RestoreReconciledBranch(ctx context.Context, gateDir, branch string, result StaleBranchReconciliation) error {
	if !result.Reconciled {
		return nil
	}
	if err := git.ValidateBareRepository(ctx, gateDir); err != nil {
		return err
	}
	if !ArchivedHeadRecorded(ctx, gateDir, branch, result.PreviousHead) {
		return fmt.Errorf("restore private mirror %q: archived head %s is unavailable", branch, result.PreviousHead)
	}
	ref := "refs/heads/" + branch
	if _, exists, err := git.DirectRefTarget(ctx, gateDir, ref); err != nil || exists {
		return err
	}
	if _, err := git.Run(ctx, gateDir, "update-ref", "--no-deref", ref, result.PreviousHead, strings.Repeat("0", len(result.PreviousHead))); err != nil {
		if _, exists, readErr := git.DirectRefTarget(ctx, gateDir, ref); readErr == nil && exists {
			return nil
		}
		return fmt.Errorf("restore private mirror %s: %w", ref, err)
	}
	return nil
}

// ArchivedHeadRecorded reports whether head is the exact commit archived for
// branch by a prior reconciliation. It is the gate's own evidence that a
// caller-reported pre-reconciliation head is genuine.
func ArchivedHeadRecorded(ctx context.Context, gateDir, branch, head string) bool {
	branch = strings.TrimSpace(branch)
	head = strings.TrimSpace(head)
	if branch == "" || head == "" {
		return false
	}
	if _, err := git.Run(ctx, gateDir, "check-ref-format", "--branch", branch); err != nil {
		return false
	}
	tag := "refs/tags/no-mistakes-abandoned/" + branch + "/" + head
	archivedHead, archived, err := git.DirectRefTarget(ctx, gateDir, tag)
	if err != nil || !archived || archivedHead != head {
		return false
	}
	objectType, err := git.Run(ctx, gateDir, "cat-file", "-t", head)
	return err == nil && objectType == "commit"
}

// privateCommitsAbsentFromLive names private-only commits lacking a matching
// per-file patch on the live side, or whose matched patch's own contribution
// cannot be proven to survive to liveHead's tip.
//
// The private side is computed first so the live scan can be bounded to the
// paths the private commits actually touch. Comparison stops at the first
// unmatched patch within each commit, but visits every private-only commit.
// A rebased live head otherwise carries every default-branch
// commit since the merge base, and hashing each of those files would cost
// thousands of git invocations to answer a question about a handful of paths.
//
// A matched patch is not yet proof of survival: a merge that used a strategy
// like "ours" can carry an ancestor whose own patch matches while discarding
// its content from every descendant's tree (see contributionSurvives). That
// proof deliberately runs the check against the SPECIFIC live commit whose
// patch matched, not a single whole-tree merge of liveHead against
// privateHead: a whole-tree merge's base is the old, possibly ancient common
// ancestor of the two divergent lines, so a file both sides "add" fresh from
// that base (privateHead's own addition, further extended on the live side by
// a later, legitimate commit - e.g. a review-fix round revising a
// just-introduced doc section) is an unresolvable add/add conflict to Git's
// merge machinery even though nothing was lost. Anchoring the comparison at
// the matched commit's own parent keeps the base real and adjacent, so a
// live-side commit that only adds to what the matched commit introduced
// merges cleanly, while one that discards or replaces it does not.
func privateCommitsAbsentFromLive(ctx context.Context, repoDir, liveHead, privateHead string) ([]string, error) {
	privateOnly, err := commitList(ctx, repoDir, "--right-only", liveHead+"..."+privateHead)
	if err != nil {
		return nil, err
	}
	if len(privateOnly) == 0 {
		return nil, nil
	}

	type privateCommit struct {
		sha        string
		patches    []string
		comparable bool
	}
	privateCommits := make([]privateCommit, 0, len(privateOnly))
	paths := make(map[string]bool)
	for _, commit := range privateOnly {
		patches, comparable, err := perFilePatchIDs(ctx, repoDir, commit)
		if err != nil {
			return nil, err
		}
		privateCommits = append(privateCommits, privateCommit{sha: commit, patches: patches, comparable: comparable})
		if !comparable {
			continue
		}
		for _, patch := range patches {
			path, _, ok := strings.Cut(patch, "\x00")
			if ok {
				paths[path] = true
			}
		}
	}

	livePatches, err := liveSidePatchIDs(ctx, repoDir, liveHead, privateHead, paths)
	if err != nil {
		return nil, err
	}

	var atRisk []string
	for _, commit := range privateCommits {
		if !commit.comparable {
			atRisk = append(atRisk, commit.sha)
			continue
		}
		remaining := make(map[string][]string, len(livePatches))
		for patch, commits := range livePatches {
			remaining[patch] = append([]string(nil), commits...)
		}
		type match struct{ path, liveCommit string }
		var matches []match
		represented := true
		for _, patch := range commit.patches {
			candidates := remaining[patch]
			if len(candidates) == 0 {
				represented = false
				break
			}
			remaining[patch] = candidates[1:]
			path, _, _ := strings.Cut(patch, "\x00")
			matches = append(matches, match{path: path, liveCommit: candidates[0]})
		}
		if !represented {
			atRisk = append(atRisk, commit.sha)
			continue
		}
		survives := true
		for _, m := range matches {
			ok, err := contributionSurvives(ctx, repoDir, liveHead, m.path, m.liveCommit)
			if err != nil {
				return nil, err
			}
			if !ok {
				survives = false
				break
			}
		}
		if !survives {
			atRisk = append(atRisk, commit.sha)
			continue
		}
		livePatches = remaining
	}
	return atRisk, nil
}

// contributionSurvives proves that matchedCommit's own patch for path is
// still reflected in liveHead's current content for that path - i.e. that
// nothing discarded or replaced it on the way to the tip.
//
// A three-way merge anchored at matchedCommit's own parent was tried first
// and rejected: for a path matchedCommit newly introduces, that parent has no
// version of it at all, so the merge base is empty exactly like the ancient,
// far-away merge-base this check exists to avoid. Git's merge machinery
// treats two additions of the same path with different content as an
// unresolvable add/add conflict regardless of whether one is a superset of
// the other, so a legitimate live-only commit that further extends the very
// file matchedCommit just added (a review-fix round, most commonly) would
// still spuriously conflict.
//
// Instead, the check reasons directly from matchedCommit's own patch, since
// matchedCommit is necessarily an ancestor of liveHead (it was drawn from the
// left-only scan of liveHead's own history) rather than a divergent sibling:
// every line the patch ADDED (relative to matchedCommit's own parent) must
// still be present in liveHead's content, and every line the patch REMOVED
// must still be absent there. A later commit that only adds more leaves both
// true; a later commit that discards or reverts the change (a merge using the
// "ours" strategy, most commonly) breaks one of them, and the caller keeps
// the private commit at risk.
func contributionSurvives(ctx context.Context, repoDir, liveHead, path, matchedCommit string) (bool, error) {
	parent, comparable, err := firstParentOrEmptyTree(ctx, repoDir, matchedCommit)
	if err != nil {
		return false, err
	}
	if !comparable {
		return false, nil
	}
	added, removed, textual, err := diffLines(ctx, repoDir, parent, matchedCommit, path)
	if err != nil {
		return false, err
	}
	if !textual {
		// A binary file's diff carries no +/- text lines to reason about, so
		// the line-presence check below would pass vacuously regardless of
		// what actually changed. Fail closed rather than silently agreeing.
		return false, nil
	}
	ours, err := blobAtPath(ctx, repoDir, liveHead, path)
	if err != nil {
		return false, err
	}
	ourLines := splitLines(ours)
	if !linesContainedWithMultiplicity(ourLines, added) {
		return false, nil
	}
	if linesAnyPresent(ourLines, removed) {
		return false, nil
	}
	return true, nil
}

// diffLines returns the lines a path-scoped diff from -> to adds and removes,
// with zero context so every returned line is a genuine addition or removal
// rather than unchanged context git included around a hunk. textual is false
// for a binary file, whose diff carries no +/- lines to parse at all.
func diffLines(ctx context.Context, repoDir, from, to, path string) (added, removed []string, textual bool, err error) {
	diff, err := git.RunRaw(ctx, repoDir, "diff", "--no-ext-diff", "-U0", from, to, "--", ":(literal)"+path)
	if err != nil {
		return nil, nil, false, err
	}
	if bytes.Contains(diff, []byte("\nBinary files ")) || strings.HasPrefix(string(diff), "Binary files ") {
		return nil, nil, false, nil
	}
	for _, line := range strings.Split(string(diff), "\n") {
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "@@"):
			continue
		case strings.HasPrefix(line, "+"):
			added = append(added, line[1:])
		case strings.HasPrefix(line, "-"):
			removed = append(removed, line[1:])
		}
	}
	return added, removed, true, nil
}

func splitLines(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
}

// linesContainedWithMultiplicity reports whether every needle appears in
// haystack at least as many times as it appears in needles.
func linesContainedWithMultiplicity(haystack, needles []string) bool {
	counts := make(map[string]int, len(haystack))
	for _, line := range haystack {
		counts[line]++
	}
	for _, needle := range needles {
		if counts[needle] == 0 {
			return false
		}
		counts[needle]--
	}
	return true
}

func linesAnyPresent(haystack, needles []string) bool {
	if len(needles) == 0 {
		return false
	}
	present := make(map[string]bool, len(haystack))
	for _, line := range haystack {
		present[line] = true
	}
	for _, needle := range needles {
		if present[needle] {
			return true
		}
	}
	return false
}

// blobAtPath returns path's content at ref, or nil when ref is the empty-tree
// sentinel or the path does not exist there.
func blobAtPath(ctx context.Context, repoDir, ref, path string) ([]byte, error) {
	if ref == git.EmptyTreeSHA {
		return nil, nil
	}
	content, err := git.RunRaw(ctx, repoDir, "show", ref+":"+path)
	if err != nil {
		// git show's only expected failure here is "path does not exist at
		// this commit" - callers already verified ref itself resolves.
		return nil, nil
	}
	return content, nil
}

// liveSidePatchIDs collects per-file patch identities from the live-only
// history, restricted to the paths the private side needs proven, keyed to
// every live commit that introduced each identity so a caller can verify
// which specific commit's contribution needs to survive to the tip.
func liveSidePatchIDs(ctx context.Context, repoDir, liveHead, privateHead string, paths map[string]bool) (map[string][]string, error) {
	livePatches := make(map[string][]string)
	if len(paths) == 0 {
		return livePatches, nil
	}
	args := []string{"--full-history", "--left-only", liveHead + "..." + privateHead, "--"}
	for path := range paths {
		args = append(args, ":(literal)"+path)
	}
	liveOnly, err := commitList(ctx, repoDir, args...)
	if err != nil {
		return nil, err
	}
	for _, commit := range liveOnly {
		patches, comparable, err := perFilePatchIDs(ctx, repoDir, commit)
		if err != nil {
			return nil, err
		}
		if !comparable {
			continue
		}
		for _, patch := range patches {
			path, _, ok := strings.Cut(patch, "\x00")
			if !ok || !paths[path] {
				continue
			}
			livePatches[patch] = append(livePatches[patch], commit)
		}
	}
	return livePatches, nil
}

func commitList(ctx context.Context, repoDir string, args ...string) ([]string, error) {
	out, err := git.Run(ctx, repoDir, append([]string{"rev-list"}, args...)...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// firstParentOrEmptyTree returns commit's first parent, or the well-known
// empty-tree SHA for a root commit. comparable is false for a merge commit,
// whose combined meaning is not safely represented by first-parent patches.
func firstParentOrEmptyTree(ctx context.Context, repoDir, commit string) (string, bool, error) {
	parentLine, err := git.Run(ctx, repoDir, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return "", false, err
	}
	parents := strings.Fields(parentLine)
	if len(parents) > 2 {
		return "", false, nil
	}
	if len(parents) == 2 {
		return parents[1], true, nil
	}
	return git.EmptyTreeSHA, true, nil
}

func perFilePatchIDs(ctx context.Context, repoDir, commit string) ([]string, bool, error) {
	parent, comparable, err := firstParentOrEmptyTree(ctx, repoDir, commit)
	if err != nil {
		return nil, false, err
	}
	if !comparable {
		return nil, false, nil
	}
	rawPaths, err := git.RunRaw(ctx, repoDir, "diff-tree", "--root", "--no-commit-id", "--name-only", "--no-renames", "-r", "-z", commit)
	if err != nil {
		return nil, false, err
	}
	var patches []string
	for _, rawPath := range strings.Split(strings.TrimSuffix(string(rawPaths), "\x00"), "\x00") {
		if rawPath == "" {
			continue
		}
		patchID, err := git.StablePatchID(ctx, repoDir, parent, commit, rawPath)
		if err != nil {
			return nil, false, err
		}
		if patchID != "" {
			patches = append(patches, rawPath+"\x00"+patchID)
		}
	}
	return patches, true, nil
}
