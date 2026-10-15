// Package status decides one person's enforcement status for one enforced
// artifact placement. It is the single definition of the truth table: callers
// gather the inputs, this package decides, and nothing is ever stored.
package status

import (
	"time"

	v1 "github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1"
)

// Reasons carried in Decision.Reason for the non-obvious outcomes.
const (
	ReasonNotDerivable    = "enforcement not supported for this ide_type"
	ReasonNeverReported   = "no completed harness sync"
	ReasonRepoNotKnown    = "no sync scope for this repository"
	failureReasonFallback = "apply failed"
)

// Observation is what the person's latest report carries at the placement's
// identity (ide_type, repository, source_path).
type Observation struct {
	ContentHash string
	ObservedAt  time.Time
}

// Failure is an apply failure the person's agent reported for the placement.
type Failure struct {
	// ContentHash is the primary hash the agent was trying to apply.
	ContentHash string
	Reason      string
	ReportedAt  time.Time
}

// Input is everything the decision needs; the caller resolves it.
type Input struct {
	Now time.Time
	// EnforceUntil is the close of the enforcement window; nil means no window.
	EnforceUntil *time.Time
	// PathErr is the install-path derivation error for the placement, if any.
	PathErr error
	// PrimaryHash is the enforced primary body's content hash.
	PrimaryHash string
	// RepoLocal marks a REPO_LOCAL placement, which also needs RepoKnown.
	RepoLocal bool
	// HasReported is true once any agent of the person has reported state.
	HasReported bool
	// RepoKnown is true when the person's report lists a checkout of the
	// placement's repository.
	RepoKnown bool
	// Observed is the report's entry at the placement identity; nil if absent.
	Observed *Observation
	// Failure is the latest apply failure for the placement; nil if none.
	Failure *Failure
}

// Decision is the computed status with its grounding.
type Decision struct {
	Status v1.EnforcementStatus
	// Reason explains EXCLUDED, FAILED and a non-derivable PENDING.
	Reason string
	// At is the report that grounds INSTALLED or the failure behind FAILED.
	At time.Time
}

// IsExpired reports whether an enforcement window has closed. A window closing
// exactly now is still open.
func IsExpired(enforceUntil *time.Time, now time.Time) bool {
	return enforceUntil != nil && enforceUntil.Before(now)
}

// Decide applies the truth table, first match wins:
//
//	window closed                           -> EXPIRED (report not consulted)
//	path not derivable                      -> PENDING with reason
//	person never reported                   -> EXCLUDED
//	repo-local and repo not known           -> EXCLUDED
//	observed hash == enforced hash          -> INSTALLED
//	failure reported for the enforced hash  -> FAILED
//	otherwise                               -> PENDING
//
// INSTALLED outranks FAILED so a stale failure cannot mask a converged
// machine, and a failure for any other hash is ignored because it describes
// content that is no longer enforced.
func Decide(in Input) Decision {
	if IsExpired(in.EnforceUntil, in.Now) {
		return Decision{Status: v1.EnforcementStatus_ENFORCEMENT_STATUS_EXPIRED}
	}
	if in.PathErr != nil {
		return Decision{Status: v1.EnforcementStatus_ENFORCEMENT_STATUS_PENDING, Reason: ReasonNotDerivable}
	}
	if !in.HasReported {
		return Decision{Status: v1.EnforcementStatus_ENFORCEMENT_STATUS_EXCLUDED, Reason: ReasonNeverReported}
	}
	if in.RepoLocal && !in.RepoKnown {
		return Decision{Status: v1.EnforcementStatus_ENFORCEMENT_STATUS_EXCLUDED, Reason: ReasonRepoNotKnown}
	}
	if in.Observed != nil && in.PrimaryHash != "" && in.Observed.ContentHash == in.PrimaryHash {
		return Decision{Status: v1.EnforcementStatus_ENFORCEMENT_STATUS_INSTALLED, At: in.Observed.ObservedAt}
	}
	if in.Failure != nil && in.PrimaryHash != "" && in.Failure.ContentHash == in.PrimaryHash {
		reason := in.Failure.Reason
		if reason == "" {
			reason = failureReasonFallback
		}
		return Decision{Status: v1.EnforcementStatus_ENFORCEMENT_STATUS_FAILED, Reason: reason, At: in.Failure.ReportedAt}
	}
	return Decision{Status: v1.EnforcementStatus_ENFORCEMENT_STATUS_PENDING}
}

// DecideRemoval grades a person who is no longer targeted: REMOVED once their
// report carries nothing at the path, REMOVAL_PENDING while any file is there.
// It tracks presence, not content, because the definition's body may have
// changed since the person last converged it. A failure the agent reported
// for the placement explains why the file is still there.
func DecideRemoval(observed *Observation, failure *Failure) Decision {
	if observed == nil {
		return Decision{Status: v1.EnforcementStatus_ENFORCEMENT_STATUS_REMOVED}
	}
	d := Decision{Status: v1.EnforcementStatus_ENFORCEMENT_STATUS_REMOVAL_PENDING, At: observed.ObservedAt}
	if failure != nil {
		d.Reason = failure.Reason
	}
	return d
}
