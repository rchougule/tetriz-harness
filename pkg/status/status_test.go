package status

import (
	"errors"
	"testing"
	"time"

	v1 "github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1"
)

const (
	installed      = v1.EnforcementStatus_ENFORCEMENT_STATUS_INSTALLED
	pending        = v1.EnforcementStatus_ENFORCEMENT_STATUS_PENDING
	failed         = v1.EnforcementStatus_ENFORCEMENT_STATUS_FAILED
	excluded       = v1.EnforcementStatus_ENFORCEMENT_STATUS_EXCLUDED
	removalPending = v1.EnforcementStatus_ENFORCEMENT_STATUS_REMOVAL_PENDING
	removed        = v1.EnforcementStatus_ENFORCEMENT_STATUS_REMOVED
	expired        = v1.EnforcementStatus_ENFORCEMENT_STATUS_EXPIRED

	enforced = "hash-enforced"
	stale    = "hash-stale"
)

var (
	now      = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	seenAt   = now.Add(-10 * time.Minute)
	failedAt = now.Add(-5 * time.Minute)
	future   = now.Add(24 * time.Hour)
	past     = now.Add(-time.Second)
)

// base is an eligible user-global placement with an open window and nothing observed.
func base() Input {
	return Input{Now: now, EnforceUntil: &future, PrimaryHash: enforced, HasReported: true}
}

func with(mut func(*Input)) Input {
	in := base()
	mut(&in)
	return in
}

// TestDecide is the truth table. Rows are grouped by the branch that decides them;
// the later rows in each group prove the earlier branches outrank the later ones.
func TestDecide(t *testing.T) {
	tests := []struct {
		name string
		in   Input
		want Decision
	}{
		// Window closed: EXPIRED for everyone, whatever the report says.
		{"expired", with(func(in *Input) { in.EnforceUntil = &past }), Decision{Status: expired}},
		{"expired outranks installed", with(func(in *Input) {
			in.EnforceUntil = &past
			in.Observed = &Observation{ContentHash: enforced, ObservedAt: seenAt}
		}), Decision{Status: expired}},
		{"expired outranks never reported", with(func(in *Input) { in.EnforceUntil = &past; in.HasReported = false }), Decision{Status: expired}},
		{"expired outranks non-derivable path", with(func(in *Input) { in.EnforceUntil = &past; in.PathErr = errors.New("x") }), Decision{Status: expired}},
		{"window closing exactly now is open", with(func(in *Input) { in.EnforceUntil = &now }), Decision{Status: pending}},
		{"no window never expires", with(func(in *Input) { in.EnforceUntil = nil }), Decision{Status: pending}},

		// Path not derivable: PENDING with a reason rather than failing the whole view.
		{"non-derivable path", with(func(in *Input) { in.PathErr = errors.New("x") }), Decision{Status: pending, Reason: ReasonNotDerivable}},
		{"non-derivable outranks never reported", with(func(in *Input) { in.PathErr = errors.New("x"); in.HasReported = false }), Decision{Status: pending, Reason: ReasonNotDerivable}},

		// Eligibility.
		{"never reported", with(func(in *Input) { in.HasReported = false }), Decision{Status: excluded, Reason: ReasonNeverReported}},
		{"never reported outranks a stray observation", with(func(in *Input) {
			in.HasReported = false
			in.Observed = &Observation{ContentHash: enforced, ObservedAt: seenAt}
		}), Decision{Status: excluded, Reason: ReasonNeverReported}},
		{"repo-local without the repo", with(func(in *Input) { in.RepoLocal = true }), Decision{Status: excluded, Reason: ReasonRepoNotKnown}},
		{"repo-local without the repo outranks installed", with(func(in *Input) {
			in.RepoLocal = true
			in.Observed = &Observation{ContentHash: enforced, ObservedAt: seenAt}
		}), Decision{Status: excluded, Reason: ReasonRepoNotKnown}},
		{"repo known is ignored for user-global", with(func(in *Input) {
			in.RepoKnown = false
			in.Observed = &Observation{ContentHash: enforced, ObservedAt: seenAt}
		}), Decision{Status: installed, At: seenAt}},

		// The observed report.
		{"installed", with(func(in *Input) { in.Observed = &Observation{ContentHash: enforced, ObservedAt: seenAt} }), Decision{Status: installed, At: seenAt}},
		{"repo-local installed", with(func(in *Input) {
			in.RepoLocal, in.RepoKnown = true, true
			in.Observed = &Observation{ContentHash: enforced, ObservedAt: seenAt}
		}), Decision{Status: installed, At: seenAt}},
		{"installed outranks a failure for the same hash", with(func(in *Input) {
			in.Observed = &Observation{ContentHash: enforced, ObservedAt: seenAt}
			in.Failure = &Failure{ContentHash: enforced, Reason: "EACCES", ReportedAt: failedAt}
		}), Decision{Status: installed, At: seenAt}},
		{"locally edited copy is pending", with(func(in *Input) { in.Observed = &Observation{ContentHash: stale, ObservedAt: seenAt} }), Decision{Status: pending}},
		{"empty enforced hash never matches", with(func(in *Input) {
			in.PrimaryHash = ""
			in.Observed = &Observation{ContentHash: "", ObservedAt: seenAt}
		}), Decision{Status: pending}},

		// Apply failures.
		{"failed for the enforced hash", with(func(in *Input) {
			in.Failure = &Failure{ContentHash: enforced, Reason: "EACCES", ReportedAt: failedAt}
		}), Decision{Status: failed, Reason: "EACCES", At: failedAt}},
		{"failed with a stale copy on disk", with(func(in *Input) {
			in.Observed = &Observation{ContentHash: stale, ObservedAt: seenAt}
			in.Failure = &Failure{ContentHash: enforced, Reason: "disk full", ReportedAt: failedAt}
		}), Decision{Status: failed, Reason: "disk full", At: failedAt}},
		{"failed without a reason gets one", with(func(in *Input) {
			in.Failure = &Failure{ContentHash: enforced, ReportedAt: failedAt}
		}), Decision{Status: failed, Reason: failureReasonFallback, At: failedAt}},
		{"failure for an older hash is stale", with(func(in *Input) {
			in.Failure = &Failure{ContentHash: stale, Reason: "EACCES", ReportedAt: failedAt}
		}), Decision{Status: pending}},
		{"repo-local failed", with(func(in *Input) {
			in.RepoLocal, in.RepoKnown = true, true
			in.Failure = &Failure{ContentHash: enforced, Reason: "hash mismatch", ReportedAt: failedAt}
		}), Decision{Status: failed, Reason: "hash mismatch", At: failedAt}},

		// Otherwise.
		{"eligible, nothing observed", base(), Decision{Status: pending}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Decide(tt.in); got != tt.want {
				t.Fatalf("Decide = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDecideRemoval(t *testing.T) {
	kept := &Failure{Reason: "left in place: modified since installed: SKILL.md", ReportedAt: seenAt}
	tests := []struct {
		name     string
		observed *Observation
		failure  *Failure
		want     Decision
	}{
		{"nothing at the path", nil, nil, Decision{Status: removed}},
		{"enforced body still present", &Observation{ContentHash: enforced, ObservedAt: seenAt}, nil, Decision{Status: removalPending, At: seenAt}},
		{"any other body still present", &Observation{ContentHash: stale, ObservedAt: seenAt}, nil, Decision{Status: removalPending, At: seenAt}},
		{"left in place carries the agent's reason", &Observation{ContentHash: stale, ObservedAt: seenAt}, kept, Decision{Status: removalPending, Reason: kept.Reason, At: seenAt}},
		{"a reason with nothing at the path is stale", nil, kept, Decision{Status: removed}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DecideRemoval(tt.observed, tt.failure); got != tt.want {
				t.Fatalf("DecideRemoval = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestIsExpired(t *testing.T) {
	tests := []struct {
		name  string
		until *time.Time
		want  bool
	}{
		{"no window", nil, false},
		{"future", &future, false},
		{"exactly now", &now, false},
		{"past", &past, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsExpired(tt.until, now); got != tt.want {
				t.Fatalf("IsExpired = %v, want %v", got, tt.want)
			}
		})
	}
}
