// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

//go:generate go tool mockgen -source=analytics.go -destination=mocks/mock_analytics.go -package=mocks

import (
	"context"

	taskexecutionsv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/taskexecutions/v1"
)

// unspecifiedLabel is the shared string for an unknown/unset enum value — the metric labels render an
// unmapped enum this way.
const unspecifiedLabel = "unspecified"

// AnalyticsEmitter emits the engine's server-side analytics events. Emission is best-effort and must
// never fail a run. The default implementation (NewNoopAnalyticsEmitter) does nothing; a host can
// inject one that forwards to its own analytics sink.
type AnalyticsEmitter interface {
	// EmitDerivationTriggered records one derivation_triggered event for a run, carrying the org, the
	// project's repo id (scope_value), the run id, and the trigger type.
	EmitDerivationTriggered(ctx context.Context, orgID, scopeValue, taskExecutionID string, trigger taskexecutionsv1.TaskTriggerType)
}

// noopAnalyticsEmitter is the default AnalyticsEmitter: it records nothing.
type noopAnalyticsEmitter struct{}

// NewNoopAnalyticsEmitter returns an AnalyticsEmitter that does nothing.
func NewNoopAnalyticsEmitter() AnalyticsEmitter { return noopAnalyticsEmitter{} }

func (noopAnalyticsEmitter) EmitDerivationTriggered(context.Context, string, string, string, taskexecutionsv1.TaskTriggerType) {
}
