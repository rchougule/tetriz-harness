// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/llmanalytics"
	pyclient "github.com/tetriz-ai/tetriz-harness/recommendations/internal/pyclient"
)

const (
	// maxCommentsPerClassifyBatch is the tetriz-py classify-themes hard cap
	// (ClassifyThemesRequest.comments max_length=3000). A larger corpus is split
	// into batches of this size and the batch-local indices re-based to global
	// indices on merge; a single over-cap request would 422.
	maxCommentsPerClassifyBatch = 3000

	// defaultLLMTimeout bounds one LLM-service call (classify_harness_intent or
	// generate_harness_fix). Intent classification is fast; generate_harness_fix is the slow path —
	// a full-body rewrite of a large committed artifact has a long model tail. 300s sits
	// strictly ABOVE the service's per-task request timeout for generate_harness_fix (285s), so the
	// service always fails first and this Go caller never cancels a live generation.
	defaultLLMTimeout = 5 * 60 * time.Second

	// llmMaxAttempts / llmRetryBaseBackoff bound the transient-retry on a tetriz-py
	// call. Only FAST transients are retried (a non-timeout connection error, HTTP
	// 429/5xx); a timeout is not retried (it already spent the per-call budget and
	// would risk the derivation StaleAfter window). 3 fast-failing attempts at
	// 500ms→1s backoff add at most ~1.5s — well under StaleAfter.
	llmMaxAttempts      = 3
	llmRetryBaseBackoff = 500 * time.Millisecond
)

// LLMClient calls the tetriz-py service for the harness recommendation derivation
// pipeline: review-theme classification (classify-themes) and artifact derivation
// (generate-content, task_type=derive_org_artifacts), over the shared tetriz-py
// transport client. There is no LLM-as-judge call — the quality gate is
// deterministic Go validators.
type LLMClient struct {
	pyClient *pyclient.Client
	// retryBaseBackoff is the first transient-retry backoff (doubled per attempt).
	// A field, not the const directly, so tests can zero it for instant retries.
	retryBaseBackoff time.Duration
	// classifyBatchSize / timeoutSeconds are live config accessors (dynamic, read
	// per call), so the batch size and per-call timeout can be tuned without a
	// redeploy. Either may be nil (tests / callers that want the built-in defaults).
	classifyBatchSize func() int
	timeoutSeconds    func() int
	// emitter records the aggregated classify-themes model_call_completed event
	//. The derive path emits via the shared client's success hook
	// instead; classify emits here because it aggregates across batches. nil
	// disables both (the hook is only wired when the emitter is set).
	emitter llmanalytics.ModelAnalyticsEmitter
}

// Option configures an LLMClient at construction.
type Option func(*LLMClient)

// WithEmitter attaches the model-call analytics emitter. Omit it (or
// pass a nil emitter) to disable emission.
func WithEmitter(emitter llmanalytics.ModelAnalyticsEmitter) Option {
	return func(c *LLMClient) { c.emitter = emitter }
}

// NewLLMClient creates a client pointing at the tetriz-py service base URL.
// classifyBatchSize and timeoutSeconds are live config accessors (may be nil to
// use the built-in defaults). The per-call timeout is applied via a context
// deadline per attempt (not the shared client's WithTimeout), so a dynamic
// change to the configured value takes effect on the next call.
func NewLLMClient(baseURL string, classifyBatchSize func() int, timeoutSeconds func() int, opts ...Option) *LLMClient {
	c := &LLMClient{
		retryBaseBackoff:  llmRetryBaseBackoff,
		classifyBatchSize: classifyBatchSize,
		timeoutSeconds:    timeoutSeconds,
	}
	for _, opt := range opts {
		opt(c)
	}
	// The derive path emits via the success hook; classify aggregates and emits
	// itself. Wire the hook only when an emitter is configured.
	var clientOpts []pyclient.Option
	if c.emitter != nil {
		clientOpts = append(clientOpts, pyclient.WithSuccessHook(llmanalytics.NewSuccessHook(c.emitter)))
	}
	c.pyClient = pyclient.New(baseURL, clientOpts...)
	return c
}

// batchSize is the live per-call classify batch size, clamped to (0,
// maxCommentsPerClassifyBatch] — the tetriz-py API hard cap. A nil accessor or
// an out-of-range value falls back to the hard cap.
func (c *LLMClient) batchSize() int {
	if c.classifyBatchSize == nil {
		return maxCommentsPerClassifyBatch
	}
	n := c.classifyBatchSize()
	if n <= 0 || n > maxCommentsPerClassifyBatch {
		return maxCommentsPerClassifyBatch
	}
	return n
}

// callTimeout is the live per-call timeout; a nil accessor or a non-positive
// value falls back to defaultLLMTimeout.
func (c *LLMClient) callTimeout() time.Duration {
	if c.timeoutSeconds != nil {
		if s := c.timeoutSeconds(); s > 0 {
			return time.Duration(s) * time.Second
		}
	}
	return defaultLLMTimeout
}

// ThemeClassification is one classified review comment: Index references the
// comment's position in the ORIGINAL ClassifyReviewThemes input (batch-local
// indices are re-based to global on merge); Theme is a theme slug or "other";
// Confidence is the classifier score (SBERT cosine or Gemini), 0..1.
type ThemeClassification struct {
	Index      int     `json:"index"`
	Theme      string  `json:"theme"`
	Confidence float64 `json:"confidence"`
}

// ClassifyResult is the merged classification of every input comment plus the
// strategy tetriz-py resolved for the org ("sbert" | "gemini").
type ClassifyResult struct {
	Classifications []ThemeClassification
	StrategyUsed    string
}

// ClassifyReviewThemes classifies review/comment bodies into engineering themes
// via tetriz-py classify-themes. The org's strategy (SBERT default, Gemini
// per-org) is resolved server-side — Go only sends org_id + comments. maxThemes
// caps themes returned per comment: 1 is the standard single-theme mapping; >1
// requests up to that many so a bulky multi-topic comment maps to each theme it
// spans (the enriched path), producing multiple Classifications sharing an Index.
// Inputs over the per-call cap are split into ≤maxCommentsPerClassifyBatch batches
// and each batch's local indices are re-based by the batch offset so the merged
// result indexes back into the original comments slice. Empty
// comments → empty result, no call.
func (c *LLMClient) ClassifyReviewThemes(ctx context.Context, orgID string, comments []string, maxThemes int) (*ClassifyResult, error) {
	if orgID == "" {
		return nil, fmt.Errorf("classify review themes: orgID cannot be empty")
	}
	if len(comments) == 0 {
		return &ClassifyResult{}, nil
	}

	merged := &ClassifyResult{Classifications: make([]ThemeClassification, 0, len(comments))}
	// Accumulate model-call analytics across batches so this whole ClassifyReviewThemes
	// call emits ONE model_call_completed event. Usage/cost sum across the
	// (Gemini) batches; SBERT batches carry neither. provider/model are org-level and
	// identical across batches — capture the first non-empty.
	var aggUsage *llmanalytics.TokenUsage
	var aggCost float64
	var haveCost bool
	var provider, model string
	// Read the live batch size once per call so a mid-corpus config change can't
	// re-shape the offset stride and skip/duplicate a slice.
	batchSize := c.batchSize()
	for offset := 0; offset < len(comments); offset += batchSize {
		end := offset + batchSize
		if end > len(comments) {
			end = len(comments)
		}
		batch, err := c.classifyBatch(ctx, orgID, comments[offset:end], maxThemes)
		if err != nil {
			return nil, fmt.Errorf("classifying review themes for org %s (batch at %d): %w", orgID, offset, err)
		}
		// Re-base batch-local indices to global indices into the original slice.
		for _, classification := range batch.Classifications {
			merged.Classifications = append(merged.Classifications, ThemeClassification{
				Index:      classification.Index + offset,
				Theme:      classification.Theme,
				Confidence: classification.Confidence,
			})
		}
		// The strategy is org-level and identical across batches; record the first.
		if merged.StrategyUsed == "" {
			merged.StrategyUsed = batch.StrategyUsed
		}
		if provider == "" {
			provider, model = batch.Provider, batch.Model
		}
		if batch.Usage != nil {
			if aggUsage == nil {
				aggUsage = &llmanalytics.TokenUsage{}
			}
			aggUsage.PromptTokens += batch.Usage.PromptTokens
			aggUsage.CompletionTokens += batch.Usage.CompletionTokens
			aggUsage.ThinkingTokens += batch.Usage.ThinkingTokens
			aggUsage.TotalTokens += batch.Usage.TotalTokens
		}
		if batch.CostUSD != nil {
			aggCost += *batch.CostUSD
			haveCost = true
		}
	}

	if c.emitter != nil && provider != "" {
		var cost *float64
		if haveCost {
			cost = &aggCost
		}
		c.emitter.EmitModelCall(ctx, llmanalytics.ModelCallEvent{
			OrgID:    orgID,
			TaskType: pyclient.TaskTypeClassifyThemes,
			Provider: provider,
			Model:    model,
			Usage:    aggUsage,
			CostUSD:  cost,
		})
	}
	return merged, nil
}

// classifyBatch sends one ≤cap batch (with transient retry) and returns its
// (batch-local-indexed) result. maxThemes is forwarded to the classify-themes call.
func (c *LLMClient) classifyBatch(ctx context.Context, orgID string, comments []string, maxThemes int) (*pyclient.ClassifyThemesResponse, error) {
	var resp *pyclient.ClassifyThemesResponse
	err := c.withRetry(ctx, func(attemptCtx context.Context) error {
		themesResp, callErr := c.pyClient.ClassifyThemes(attemptCtx, pyclient.ClassifyThemesRequest{OrgID: orgID, Comments: comments, MaxThemesPerComment: maxThemes})
		if callErr != nil {
			return fmt.Errorf("classify-themes call: %w", callErr)
		}
		resp = themesResp
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("classify: %w", err)
	}
	return resp, nil
}

// DeriveArtifacts calls tetriz-py generate-content (task_type=derive_org_artifacts)
// with the aggregated context and returns the parsed rules/templates/skills the
// quality gate then grades, AND the raw normalized response bytes (the Result
// envelope field verbatim) for the engine to persist as the run's audit blob.
// sourceID is the task_execution id (bound into tetriz-py logs for
// correlation). The response Result is the already schema-normalized
// DeriveOrgArtifactsResponse, so a malformed model output surfaces as a non-2xx
// (HTTP error) here, not as a partial DerivationResult. The model-call analytics
// event is emitted by the shared client's success hook.
func (c *LLMClient) DeriveArtifacts(ctx context.Context, aggCtx *promptlibraryv1.AggregatedContext, orgID, sourceID string) (*DerivationResult, []byte, error) {
	if orgID == "" {
		return nil, nil, fmt.Errorf("derive artifacts: orgID cannot be empty")
	}
	if aggCtx == nil {
		return nil, nil, fmt.Errorf("derive artifacts for org %s: aggregated context cannot be nil", orgID)
	}

	// UseProtoNames emits snake_case field names (repo_trees, high_vs_low_gap,
	// low_confidence, …) — the names the derive system prompt instructs the model
	// to read. protojson's default lowerCamelCase would not match the prompt, so
	// the model could miss context fields.
	contentBytes, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(aggCtx)
	if err != nil {
		return nil, nil, fmt.Errorf("marshalling aggregated context for org %s: %w", orgID, err)
	}

	var resp *pyclient.GenerateContentResponse
	err = c.withRetry(ctx, func(attemptCtx context.Context) error {
		contentResp, callErr := c.pyClient.GenerateContent(attemptCtx, pyclient.GenerateContentRequest{
			Content:  string(contentBytes),
			TaskType: pyclient.TaskTypeDeriveOrgArtifacts,
			OrgID:    orgID,
			SourceID: sourceID,
		})
		if callErr != nil {
			return fmt.Errorf("generate-content call: %w", callErr)
		}
		resp = contentResp
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("derive for org %s: %w", orgID, err)
	}

	var result DerivationResult
	if err := json.Unmarshal([]byte(resp.Result), &result); err != nil {
		return nil, nil, fmt.Errorf("unmarshalling derive result for org %s: %w", orgID, err)
	}

	// Return the normalized Result bytes verbatim as the audit blob — the exact
	// JSON tetriz-py emitted, not a re-marshal of `result` (which would drop any
	// field the Go DerivationResult struct does not model).
	return &result, []byte(resp.Result), nil
}

// HarnessIntentInput is the classify_harness_intent request payload: it is
// JSON-marshalled into the generate-content `content` field, and its json tags
// (`entity_type`/`stated_purpose`/`content`) match the fields the tetriz-py
// system prompt reads. EntityType is one of skill|subagent|command; StatedPurpose
// is the artifact's frontmatter description or one-line trigger; Content is its
// full file body.
type HarnessIntentInput struct {
	EntityType    string `json:"entity_type"`
	StatedPurpose string `json:"stated_purpose"`
	Content       string `json:"content"`
}

// HarnessIntentResult is the parsed classify_harness_intent response. IntentClass
// is one of mechanical|exploratory|review|orchestrator|gate|ambiguous (validated
// server-side, so a non-conforming model output surfaces as an HTTP error here,
// never a partial result); Confidence is 0..1; Rationale is a one-sentence
// justification.
type HarnessIntentResult struct {
	IntentClass string  `json:"intent_class"`
	Confidence  float64 `json:"confidence"`
	Rationale   string  `json:"rationale"`
}

// ClassifyHarnessIntent calls tetriz-py generate-content
// (task_type=classify_harness_intent) to assign one intent class to a single
// harness artifact and returns the parsed result. sourceID is the artifact
// identifier (bound into tetriz-py logs for correlation). A malformed
// model output is rejected server-side and surfaces as an HTTP 502 → the shared
// client's KindServer error → withRetry retries it; a persistently-bad response
// returns the error to the caller, which owns the fallback policy (degrading it to
// `ambiguous`). This wrapper performs only the call and schema-shaped parse.
func (c *LLMClient) ClassifyHarnessIntent(ctx context.Context, orgID, sourceID string, input HarnessIntentInput) (HarnessIntentResult, error) {
	if orgID == "" {
		return HarnessIntentResult{}, fmt.Errorf("classify harness intent: orgID cannot be empty")
	}

	marshalled, err := json.Marshal(input)
	if err != nil {
		return HarnessIntentResult{}, fmt.Errorf("marshalling harness-intent input for org %s source %s: %w", orgID, sourceID, err)
	}

	var resp *pyclient.GenerateContentResponse
	err = c.withRetry(ctx, func(attemptCtx context.Context) error {
		contentResp, callErr := c.pyClient.GenerateContent(attemptCtx, pyclient.GenerateContentRequest{
			Content:  string(marshalled),
			TaskType: pyclient.TaskTypeClassifyHarnessIntent,
			OrgID:    orgID,
			SourceID: sourceID,
		})
		if callErr != nil {
			return fmt.Errorf("generate-content call: %w", callErr)
		}
		resp = contentResp
		return nil
	})
	if err != nil {
		return HarnessIntentResult{}, fmt.Errorf("classify harness intent for org %s source %s: %w", orgID, sourceID, err)
	}

	var result HarnessIntentResult
	if err := json.Unmarshal([]byte(resp.Result), &result); err != nil {
		return HarnessIntentResult{}, fmt.Errorf("unmarshalling harness-intent result for org %s source %s: %w", orgID, sourceID, err)
	}
	return result, nil
}

// HarnessFixInput is the generate_harness_fix request payload: it is JSON-marshalled
// into the generate-content `content` field, and its json tags match the fields the
// tetriz-py system prompt reads. Dimension is the finding's dimension + detector
// family; Title is the one-line problem statement; ReferencePoint is the offending
// detail (broken path, restated rule-ID, …); CurrentContent is the artifact's full
// committed body; RepoPaths is the committed repo file-path set the model may cite;
// EntityType and ArtifactName are trace-only context.
type HarnessFixInput struct {
	Dimension      string   `json:"dimension"`
	Title          string   `json:"title"`
	ReferencePoint string   `json:"reference_point"`
	CurrentContent string   `json:"current_content"`
	RepoPaths      []string `json:"repo_paths"`
	EntityType     string   `json:"entity_type"`
	ArtifactName   string   `json:"artifact_name"`
}

// HarnessFixResult is the parsed generate_harness_fix response. SuggestedContent is
// the full modified body (not a diff), validated non-empty server-side so a
// non-conforming model output surfaces as an HTTP error here, never a partial result;
// ChangeSummary is a one-sentence description of the edit; CitedPaths are the repo
// paths the new body references (the caller grounds them against the repo path set).
type HarnessFixResult struct {
	SuggestedContent string   `json:"suggested_content"`
	ChangeSummary    string   `json:"change_summary"`
	CitedPaths       []string `json:"cited_paths"`
	// BaselineContent is the committed body the fix was computed FROM — the "before" of the
	// before/after diff the serving panel renders. Set by the engine from the scan's fetched
	// committed body, NOT by the model (json:"-"), so it is the exact baseline the SuggestedContent
	// replaces. Empty for a fix with no fetched baseline (the panel then shows only the suggestion).
	BaselineContent string `json:"-"`
	// Generated marks a fix that CREATES an artifact that does not exist yet (the org coverage-gap
	// guard hook, an mcp config suggestion) rather than editing a committed body. Its BaselineContent
	// is legitimately empty — there is nothing to replace — so attachGroundedFixes keeps the diff's
	// "before" empty (an all-added "new content" render) instead of the fetch-failed fallback that
	// copies the suggestion into "before" to show an empty diff. Model does not set it (json:"-").
	Generated bool `json:"-"`
}

// GenerateHarnessFix calls tetriz-py generate-content
// (task_type=generate_harness_fix) to draft a corrected body for one content-rewrite
// finding and returns the parsed result. sourceID is the artifact identifier (bound
// into tetriz-py logs for correlation). A malformed model output is
// rejected server-side and surfaces as an HTTP 502 → the shared client's KindServer
// error → withRetry retries it; a persistently-bad response returns the error to the
// caller, which owns the fallback policy (degrading to no fix). This wrapper performs
// only the call and schema-shaped parse.
func (c *LLMClient) GenerateHarnessFix(ctx context.Context, orgID, sourceID string, input HarnessFixInput) (HarnessFixResult, error) {
	if orgID == "" {
		return HarnessFixResult{}, fmt.Errorf("generate harness fix: orgID cannot be empty")
	}

	marshalled, err := json.Marshal(input)
	if err != nil {
		return HarnessFixResult{}, fmt.Errorf("marshalling harness-fix input for org %s source %s: %w", orgID, sourceID, err)
	}

	var resp *pyclient.GenerateContentResponse
	err = c.withRetry(ctx, func(attemptCtx context.Context) error {
		contentResp, callErr := c.pyClient.GenerateContent(attemptCtx, pyclient.GenerateContentRequest{
			Content:  string(marshalled),
			TaskType: pyclient.TaskTypeGenerateHarnessFix,
			OrgID:    orgID,
			SourceID: sourceID,
		})
		if callErr != nil {
			return fmt.Errorf("generate-content call: %w", callErr)
		}
		resp = contentResp
		return nil
	})
	if err != nil {
		return HarnessFixResult{}, fmt.Errorf("generate harness fix for org %s source %s: %w", orgID, sourceID, err)
	}

	var result HarnessFixResult
	if err := json.Unmarshal([]byte(resp.Result), &result); err != nil {
		return HarnessFixResult{}, fmt.Errorf("unmarshalling harness-fix result for org %s source %s: %w", orgID, sourceID, err)
	}
	return result, nil
}

// withRetry runs call, retrying a FAST transient failure (a non-timeout
// connection error, or HTTP 429/5xx) up to llmMaxAttempts with backoff. A 4xx, a
// parse-level error, and a timeout are NOT retried — a 4xx is terminal, and a
// timeout already spent the per-call budget and would risk the derivation run's
// StaleAfter window. Each attempt is bounded by the live per-call timeout via a
// context deadline (so a dynamic config change applies on the next call); the
// parent ctx's deadline still caps it.
func (c *LLMClient) withRetry(ctx context.Context, call func(attemptCtx context.Context) error) error {
	var lastErr error
	attempts := 0
	for attempts < llmMaxAttempts {
		if attempts > 0 {
			backoff := c.retryBaseBackoff << (attempts - 1) // 500ms, 1s, …
			select {
			case <-ctx.Done():
				return fmt.Errorf("aborted before retry %d: %w", attempts, ctx.Err())
			case <-time.After(backoff):
			}
		}
		attempts++
		attemptCtx, cancel := context.WithTimeout(ctx, c.callTimeout())
		err := call(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable(err) {
			break // terminal (4xx / parse / timeout) — don't burn the remaining attempts
		}
	}
	return fmt.Errorf("after %d attempt(s): %w", attempts, lastErr)
}

// retryable reports whether a shared-client error is a fast transient worth
// retrying: an upstream rate-limit (429), a 5xx, or a connection-level transport
// fault. A timeout, a 4xx, a parse failure, and an over-ceiling body are not
// retried.
func retryable(err error) bool {
	var we *pyclient.Error
	if !errors.As(err, &we) {
		return false
	}
	switch we.Kind {
	case pyclient.KindTooManyRequests, pyclient.KindServer, pyclient.KindTransport:
		return true
	default:
		return false
	}
}
