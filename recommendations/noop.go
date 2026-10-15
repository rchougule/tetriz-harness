// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"context"
	"errors"

	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
)

// errLLMNotConfigured is returned by every NoopLLMClient leg. The engine treats an LLM error as a
// graceful degrade: a DERIVED run fails (it needs the derive leg), while a DETECTED run's behavioral
// intent classification and fix drafting degrade — the config-only detectors still produce and persist
// their findings. This is what lets the zero-setup on-disk DETECTED demo run with no tetriz-py.
var errLLMNotConfigured = errors.New("llm client not configured (no tetriz-py url)")

// NoopLLMClient is the injectable no-op LLM used when no tetriz-py URL is configured. It satisfies the
// engine's artifactDeriver, harnessIntentClassifier, and harnessFixGenerator seams by returning a
// not-configured error from each leg, so every LLM-dependent step degrades rather than calling out.
type NoopLLMClient struct{}

// NewNoopLLMClient builds a no-op LLM client.
func NewNoopLLMClient() NoopLLMClient { return NoopLLMClient{} }

// DeriveArtifacts always errors — the DERIVED pipeline requires a real derive leg, so a no-op run fails.
func (NoopLLMClient) DeriveArtifacts(_ context.Context, _ *promptlibraryv1.AggregatedContext, _, _ string) (*DerivationResult, []byte, error) {
	return nil, nil, errLLMNotConfigured
}

// ClassifyHarnessIntent always errors — the DETECTED behavioral lane degrades (its partition is excluded
// from the evaluated set), leaving the config-only findings intact.
func (NoopLLMClient) ClassifyHarnessIntent(_ context.Context, _, _ string, _ HarnessIntentInput) (HarnessIntentResult, error) {
	return HarnessIntentResult{}, errLLMNotConfigured
}

// GenerateHarnessFix always errors — a finding keeps its evidence but gets no grounded fix (advisory).
func (NoopLLMClient) GenerateHarnessFix(_ context.Context, _, _ string, _ HarnessFixInput) (HarnessFixResult, error) {
	return HarnessFixResult{}, errLLMNotConfigured
}
