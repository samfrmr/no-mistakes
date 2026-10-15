package pipeline

import "github.com/kunchenguid/no-mistakes/internal/types"

// IsCoverageOnlyReviewGate reports whether a review gate's findings leave
// incomplete coverage as the only reason it is parked.
//
// ReviewStep parks a round for exactly two reasons: a blocking (error or
// warning) finding, or a clean round whose reviewed_paths record does not cover
// every trusted reviewable file. A payload with no blocking finding therefore
// is the coverage park. That gate has nothing for a fixer to fix, which is why
// `fix` refuses it and approve used to be the only response - a waiver of files
// nobody read. types.ActionRereview is the supported way to obtain the missing
// coverage, and this predicate is what scopes it: a gate that carries a
// blocking finding (a review question and a protected-path refusal included) is
// decided by the response that finding asks for, never by re-reviewing.
//
// An empty payload is a zero-finding gate and qualifies; one that cannot be
// parsed does not, so the response fails closed instead of being applied to
// state this code could not read.
func IsCoverageOnlyReviewGate(findingsJSON string) bool {
	if findingsJSON == "" {
		return true
	}
	findings, err := types.ParseFindingsJSON(findingsJSON)
	if err != nil {
		return false
	}
	for _, item := range findings.Items {
		if item.Severity == types.FindingSeverityError || item.Severity == types.FindingSeverityWarning {
			return false
		}
	}
	return true
}
