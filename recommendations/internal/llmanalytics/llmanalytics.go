// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package llmanalytics is the model-usage analytics seam for the recommendations engine: it defines
// the per-call event shape and the emitter interface, and adapts the pyclient success hook onto it.
// It registers nothing at import and has no default sink — a host injects an emitter; absent one,
// no event is emitted.
package llmanalytics

//go:generate go tool mockgen -source=llmanalytics.go -destination=mocks/mock_llmanalytics.go -package=mocks

import (
	"context"

	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/pyclient"
)

// TokenUsage is the token breakdown of one model call. Absent (nil) for a token-free local model.
type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	// ThinkingTokens are reasoning tokens (billed at the output rate but reported separately).
	ThinkingTokens int
	TotalTokens    int
}

// TokenUsageFrom builds a TokenUsage from a response's token counts, so the field mapping lives in
// one place.
func TokenUsageFrom(promptTokens, completionTokens, thinkingTokens, totalTokens int) *TokenUsage {
	return &TokenUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		ThinkingTokens:   thinkingTokens,
		TotalTokens:      totalTokens,
	}
}

// ModelCallEvent is one model invocation to record. Usage is nil for token-free models; CostUSD is
// nil for models with no vendor bill.
type ModelCallEvent struct {
	OrgID    string
	TaskType string
	Provider string
	Model    string
	SourceID string
	Usage    *TokenUsage
	CostUSD  *float64
}

// ModelAnalyticsEmitter records per-model-call analytics. Emission is best-effort: an implementation
// never returns an error and never fails the caller.
type ModelAnalyticsEmitter interface {
	// EmitModelCall records exactly one event for a completed model invocation.
	EmitModelCall(ctx context.Context, event ModelCallEvent)
}

// NewSuccessHook adapts a ModelAnalyticsEmitter to the pyclient SuccessHook, so a caller can wire the
// client to emit one event per successful call. This adapter is the one place the response fields map
// onto the analytics event; the dependency points one way (this package imports pyclient, never the
// reverse).
func NewSuccessHook(emitter ModelAnalyticsEmitter) pyclient.SuccessHook {
	return successHook{emitter: emitter}
}

type successHook struct {
	emitter ModelAnalyticsEmitter
}

func (h successHook) OnGenerateContent(ctx context.Context, req pyclient.GenerateContentRequest, resp *pyclient.GenerateContentResponse) {
	h.emitter.EmitModelCall(ctx, ModelCallEvent{
		OrgID:    req.OrgID,
		TaskType: req.TaskType,
		SourceID: req.SourceID,
		Provider: resp.Provider,
		Model:    resp.Model,
		Usage:    usageFromWire(resp.Usage),
		CostUSD:  resp.CostUSD,
	})
}

// usageFromWire maps the client's wire token usage onto the analytics TokenUsage, returning nil when
// the response carried none.
func usageFromWire(u *pyclient.TokenUsage) *TokenUsage {
	if u == nil {
		return nil
	}
	return TokenUsageFrom(u.PromptTokens, u.CompletionTokens, u.ThinkingTokens, u.TotalTokens)
}
