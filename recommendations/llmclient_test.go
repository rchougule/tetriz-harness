// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/llmanalytics"
	llmanalyticsmocks "github.com/tetriz-ai/tetriz-harness/recommendations/internal/llmanalytics/mocks"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/pyclient"
)

// TestClassifyReviewThemes_Chunking proves the I9 contract: a corpus over the
// per-call cap is split into ≤maxCommentsPerClassifyBatch batches and the
// batch-local indices are re-based to globally-correct indices on merge.
func TestClassifyReviewThemes_Chunking(t *testing.T) {
	t.Parallel()
	var batchSizes []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req pyclient.ClassifyThemesRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		// The server must never receive an over-cap batch (it would 422 in prod).
		assert.LessOrEqual(t, len(req.Comments), maxCommentsPerClassifyBatch)
		batchSizes = append(batchSizes, len(req.Comments))
		resp := pyclient.ClassifyThemesResponse{StrategyUsed: "sbert"}
		// Echo BATCH-LOCAL indices (0..len-1) so re-basing is what makes the merged
		// indices line up with the original slice.
		for localIndex := range req.Comments {
			resp.Classifications = append(resp.Classifications, pyclient.ThemeClassification{
				Index:      localIndex,
				Theme:      "testing",
				Confidence: 0.9,
			})
		}
		assert.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
	defer server.Close()

	const total = 6000
	comments := make([]string, total)
	for i := range comments {
		comments[i] = fmt.Sprintf("comment-%d", i)
	}

	result, err := NewLLMClient(server.URL, nil, nil).ClassifyReviewThemes(context.Background(), "org-1", comments, 1)
	require.NoError(t, err)

	require.Len(t, result.Classifications, total)
	for i, classification := range result.Classifications {
		require.Equal(t, i, classification.Index, "merged index %d must equal its global position", i)
	}
	assert.Equal(t, []int{maxCommentsPerClassifyBatch, total - maxCommentsPerClassifyBatch}, batchSizes,
		"input must be split into ≤cap batches")
	assert.Equal(t, "sbert", result.StrategyUsed)
}

func TestClassifyReviewThemes_SingleBatchAndEmpty(t *testing.T) {
	t.Parallel()
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req pyclient.ClassifyThemesRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		resp := pyclient.ClassifyThemesResponse{StrategyUsed: "gemini"}
		for localIndex := range req.Comments {
			resp.Classifications = append(resp.Classifications, pyclient.ThemeClassification{Index: localIndex, Theme: "security", Confidence: 0.5})
		}
		assert.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
	defer server.Close()
	client := NewLLMClient(server.URL, nil, nil)

	// Empty input short-circuits: no HTTP call, empty result.
	empty, err := client.ClassifyReviewThemes(context.Background(), "org-1", nil, 1)
	require.NoError(t, err)
	assert.Empty(t, empty.Classifications)
	assert.Equal(t, 0, calls)

	// A sub-cap batch is sent as one request with indices intact.
	result, err := client.ClassifyReviewThemes(context.Background(), "org-1", []string{"a", "b", "c"}, 1)
	require.NoError(t, err)
	require.Len(t, result.Classifications, 3)
	assert.Equal(t, 1, calls)
	assert.Equal(t, "gemini", result.StrategyUsed)
}

// TestLLMClient_batchSize pins the live-config clamp: the batch size
// is read from the accessor and clamped to (0, maxCommentsPerClassifyBatch] — a nil
// accessor, a zero/negative value, or an over-the-API-cap value all fall back to the
// hard cap so a misconfig can never send an over-cap batch (which tetriz-py 422s).
func TestLLMClient_batchSize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		accessor func() int
		want     int
	}{
		{name: "nil accessor falls back to the hard cap", accessor: nil, want: maxCommentsPerClassifyBatch},
		{name: "zero falls back to the hard cap", accessor: func() int { return 0 }, want: maxCommentsPerClassifyBatch},
		{name: "negative falls back to the hard cap", accessor: func() int { return -5 }, want: maxCommentsPerClassifyBatch},
		{name: "over the API cap clamps down to the hard cap", accessor: func() int { return maxCommentsPerClassifyBatch + 1 }, want: maxCommentsPerClassifyBatch},
		{name: "valid in-range value is used verbatim", accessor: func() int { return 300 }, want: 300},
		{name: "exactly the cap is allowed", accessor: func() int { return maxCommentsPerClassifyBatch }, want: maxCommentsPerClassifyBatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := NewLLMClient("http://unused", tt.accessor, nil)
			assert.Equal(t, tt.want, client.batchSize())
		})
	}
}

// TestLLMClient_callTimeout pins the live-config timeout: a nil
// accessor or a non-positive value falls back to defaultLLMTimeout; a positive value
// is taken as seconds.
func TestLLMClient_callTimeout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		accessor func() int
		want     time.Duration
	}{
		{name: "nil accessor falls back to default", accessor: nil, want: defaultLLMTimeout},
		{name: "zero falls back to default", accessor: func() int { return 0 }, want: defaultLLMTimeout},
		{name: "negative falls back to default", accessor: func() int { return -1 }, want: defaultLLMTimeout},
		{name: "positive value is taken as seconds", accessor: func() int { return 60 }, want: 60 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := NewLLMClient("http://unused", nil, tt.accessor)
			assert.Equal(t, tt.want, client.callTimeout())
		})
	}
}

// TestClassifyReviewThemes_ConfigDrivenBatchSize proves the chunking honours the
// live batch-size accessor, not just the hard-cap constant: a corpus of 5 with a
// configured batch of 2 is split [2,2,1] and the merged indices stay global.
func TestClassifyReviewThemes_ConfigDrivenBatchSize(t *testing.T) {
	t.Parallel()
	var batchSizes []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req pyclient.ClassifyThemesRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		batchSizes = append(batchSizes, len(req.Comments))
		resp := pyclient.ClassifyThemesResponse{StrategyUsed: "sbert"}
		for localIndex := range req.Comments {
			resp.Classifications = append(resp.Classifications, pyclient.ThemeClassification{Index: localIndex, Theme: "testing", Confidence: 0.9})
		}
		assert.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
	defer server.Close()

	comments := []string{"a", "b", "c", "d", "e"}
	client := NewLLMClient(server.URL, func() int { return 2 }, nil)
	result, err := client.ClassifyReviewThemes(context.Background(), "org-1", comments, 1)
	require.NoError(t, err)

	assert.Equal(t, []int{2, 2, 1}, batchSizes, "the configured batch size of 2 drives the split, not the hard cap")
	require.Len(t, result.Classifications, len(comments))
	for i, classification := range result.Classifications {
		require.Equal(t, i, classification.Index, "merged index %d must equal its global position", i)
	}
}

func TestClassifyReviewThemes_HTTPError(t *testing.T) {
	t.Parallel()
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		http.Error(w, `{"detail":"boom"}`, http.StatusInternalServerError)
	}))
	defer server.Close()
	client := NewLLMClient(server.URL, nil, nil)
	client.retryBaseBackoff = 0 // no real sleeps in the test
	_, err := client.ClassifyReviewThemes(context.Background(), "org-1", []string{"a"}, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 500")
	// A persistent 5xx is transient → retried up to the cap before failing.
	assert.Equal(t, llmMaxAttempts, calls, "a 5xx must be retried up to the attempt cap")
}

// TestPostJSONWithRetry covers the transient-vs-terminal retry policy: a fast 5xx
// or 429 is retried and a later 200 succeeds; a 4xx is terminal (no retry).
func TestPostJSONWithRetry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		statuses    []int // status returned on call 1, 2, 3…
		wantCalls   int
		wantErr     bool
		wantErrText string
	}{
		{name: "5xx then success retries once", statuses: []int{http.StatusInternalServerError, http.StatusOK}, wantCalls: 2},
		{name: "429 then success retries once", statuses: []int{http.StatusTooManyRequests, http.StatusOK}, wantCalls: 2},
		{name: "two 5xx then success", statuses: []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusOK}, wantCalls: 3},
		{name: "4xx is terminal, not retried", statuses: []int{http.StatusBadRequest, http.StatusOK}, wantCalls: 1, wantErr: true, wantErrText: "HTTP 400"},
		{name: "persistent 5xx exhausts the cap", statuses: []int{http.StatusInternalServerError, http.StatusInternalServerError, http.StatusInternalServerError, http.StatusOK}, wantCalls: llmMaxAttempts, wantErr: true, wantErrText: "HTTP 500"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				status := tt.statuses[len(tt.statuses)-1]
				if calls < len(tt.statuses) {
					status = tt.statuses[calls]
				}
				calls++
				if status == http.StatusOK {
					assert.NoError(t, json.NewEncoder(w).Encode(pyclient.ClassifyThemesResponse{StrategyUsed: "sbert"}))
					return
				}
				http.Error(w, `{"detail":"x"}`, status)
			}))
			defer server.Close()
			client := NewLLMClient(server.URL, nil, nil)
			client.retryBaseBackoff = 0

			_, err := client.classifyBatch(context.Background(), "org-1", []string{"a"}, 1)
			assert.Equal(t, tt.wantCalls, calls)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrText)
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestClassifyReviewThemes_EmitsAggregatedModelCall proves the Gemini path emits
// exactly ONE model_call_completed event per ClassifyReviewThemes call, with token
// usage AND cost summed across the Go-side batches.
func TestClassifyReviewThemes_EmitsAggregatedModelCall(t *testing.T) {
	t.Parallel()
	cost := 0.001
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req pyclient.ClassifyThemesRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		resp := pyclient.ClassifyThemesResponse{
			StrategyUsed: "gemini",
			Provider:     "gemini",
			Model:        "gemini-2.5-flash",
			Usage:        &pyclient.TokenUsage{PromptTokens: 10, CompletionTokens: 5, ThinkingTokens: 2, TotalTokens: 17},
			CostUSD:      &cost,
		}
		for localIndex := range req.Comments {
			resp.Classifications = append(resp.Classifications, pyclient.ThemeClassification{Index: localIndex, Theme: "testing", Confidence: 0.9})
		}
		assert.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
	defer server.Close()

	ctrl := gomock.NewController(t)
	emitter := llmanalyticsmocks.NewMockModelAnalyticsEmitter(ctrl)
	var got llmanalytics.ModelCallEvent
	// Exactly one event for the whole call, regardless of batch count.
	emitter.EXPECT().EmitModelCall(gomock.Any(), gomock.Any()).
		Do(func(_ context.Context, ev llmanalytics.ModelCallEvent) { got = ev }).Times(1)

	// maxCommentsPerClassifyBatch+1 comments → exactly 2 Go-side batches.
	comments := make([]string, maxCommentsPerClassifyBatch+1)
	for i := range comments {
		comments[i] = fmt.Sprintf("c-%d", i)
	}
	client := NewLLMClient(server.URL, nil, nil, WithEmitter(emitter))
	_, err := client.ClassifyReviewThemes(context.Background(), "org-1", comments, 1)
	require.NoError(t, err)

	assert.Equal(t, "org-1", got.OrgID)
	assert.Equal(t, pyclient.TaskTypeClassifyThemes, got.TaskType)
	assert.Equal(t, "gemini", got.Provider)
	assert.Equal(t, "gemini-2.5-flash", got.Model)
	require.NotNil(t, got.Usage)
	// Two batches summed.
	assert.Equal(t, 34, got.Usage.TotalTokens)
	assert.Equal(t, 20, got.Usage.PromptTokens)
	assert.Equal(t, 4, got.Usage.ThinkingTokens)
	require.NotNil(t, got.CostUSD)
	assert.InDelta(t, 0.002, *got.CostUSD, 1e-9)
}

// TestClassifyHarnessIntent covers ClassifyHarnessIntent: the happy path marshals
// the HarnessIntentInput as the generate-content `content`, sets the
// classify_harness_intent task type, and parses the result; a persistent 502
// surfaces as a retryable-kind error after the attempt cap; a 200 whose body is not
// valid JSON surfaces as a parse error. Degrading a persistently-bad response to
// `ambiguous` is the caller's job, not this wrapper's.
func TestClassifyHarnessIntent(t *testing.T) {
	t.Parallel()

	t.Run("happy path marshals input and parses result", func(t *testing.T) {
		t.Parallel()
		var gotReq pyclient.GenerateContentRequest
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
			resp := pyclient.GenerateContentResponse{
				Result:   `{"intent_class":"orchestrator","confidence":0.9,"rationale":"x"}`,
				TaskType: gotReq.TaskType,
			}
			assert.NoError(t, json.NewEncoder(w).Encode(resp))
		}))
		defer server.Close()

		client := NewLLMClient(server.URL, nil, nil)
		input := HarnessIntentInput{EntityType: "skill", StatedPurpose: "drain a queue", Content: "full body"}
		result, err := client.ClassifyHarnessIntent(context.Background(), "org-1", "src-1", input)
		require.NoError(t, err)

		// The task type + the marshalled input reached the server verbatim.
		assert.Equal(t, pyclient.TaskTypeClassifyHarnessIntent, gotReq.TaskType)
		assert.Equal(t, "org-1", gotReq.OrgID)
		assert.Equal(t, "src-1", gotReq.SourceID)
		var sentInput HarnessIntentInput
		require.NoError(t, json.Unmarshal([]byte(gotReq.Content), &sentInput))
		assert.Equal(t, input, sentInput)

		assert.Equal(t, "orchestrator", result.IntentClass)
		assert.InDelta(t, 0.9, result.Confidence, 1e-9)
		assert.Equal(t, "x", result.Rationale)
	})

	t.Run("empty orgID is rejected without a call", func(t *testing.T) {
		t.Parallel()
		var calls int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()
		client := NewLLMClient(server.URL, nil, nil)
		_, err := client.ClassifyHarnessIntent(context.Background(), "", "src-1", HarnessIntentInput{})
		require.Error(t, err)
		assert.Equal(t, 0, calls)
	})

	t.Run("persistent 502 retries then returns an error", func(t *testing.T) {
		t.Parallel()
		var calls int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			http.Error(w, `{"detail":"classify_harness_intent output invalid: ..."}`, http.StatusBadGateway)
		}))
		defer server.Close()
		client := NewLLMClient(server.URL, nil, nil)
		client.retryBaseBackoff = 0 // no real sleeps in the test

		_, err := client.ClassifyHarnessIntent(context.Background(), "org-1", "src-1",
			HarnessIntentInput{EntityType: "skill", StatedPurpose: "x", Content: "y"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "HTTP 502")
		// A 502 is KindServer → retryable → retried up to the attempt cap.
		assert.Equal(t, llmMaxAttempts, calls, "a 502 must be retried up to the attempt cap")
	})

	t.Run("200 with non-JSON result surfaces as a parse error", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			resp := pyclient.GenerateContentResponse{Result: "not json", TaskType: pyclient.TaskTypeClassifyHarnessIntent}
			assert.NoError(t, json.NewEncoder(w).Encode(resp))
		}))
		defer server.Close()
		client := NewLLMClient(server.URL, nil, nil)

		_, err := client.ClassifyHarnessIntent(context.Background(), "org-1", "src-1",
			HarnessIntentInput{EntityType: "skill", StatedPurpose: "x", Content: "y"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unmarshalling harness-intent result")
	})
}

// TestGenerateHarnessFix covers GenerateHarnessFix: the happy path marshals the
// HarnessFixInput as the generate-content `content`, sets the generate_harness_fix task
// type, and parses the result; empty orgID makes no call; a persistent 502 surfaces as
// a retryable-kind error after the attempt cap; a 200 whose body is not valid JSON
// surfaces as a parse error. Degrading a persistently-bad response to no fix is the
// caller's job, not this wrapper's.
func TestGenerateHarnessFix(t *testing.T) {
	t.Parallel()

	t.Run("happy path marshals input and parses result", func(t *testing.T) {
		t.Parallel()
		var gotReq pyclient.GenerateContentRequest
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
			resp := pyclient.GenerateContentResponse{
				Result:   `{"suggested_content":"see docs/here.md","change_summary":"repointed the dead path","cited_paths":["docs/here.md"]}`,
				TaskType: gotReq.TaskType,
			}
			assert.NoError(t, json.NewEncoder(w).Encode(resp))
		}))
		defer server.Close()

		client := NewLLMClient(server.URL, nil, nil)
		input := HarnessFixInput{
			Dimension:      "CONFLICT / broken-path",
			Title:          "Instruction references a path that does not exist",
			ReferencePoint: "docs/gone.md",
			CurrentContent: "see docs/gone.md",
			RepoPaths:      []string{"docs/here.md"},
			EntityType:     "skill",
			ArtifactName:   "x",
		}
		result, err := client.GenerateHarnessFix(context.Background(), "org-1", "src-1", input)
		require.NoError(t, err)

		// The task type + the marshalled input reached the server verbatim.
		assert.Equal(t, pyclient.TaskTypeGenerateHarnessFix, gotReq.TaskType)
		assert.Equal(t, "org-1", gotReq.OrgID)
		assert.Equal(t, "src-1", gotReq.SourceID)
		var sentInput HarnessFixInput
		require.NoError(t, json.Unmarshal([]byte(gotReq.Content), &sentInput))
		assert.Equal(t, input, sentInput)

		assert.Equal(t, "see docs/here.md", result.SuggestedContent)
		assert.Equal(t, "repointed the dead path", result.ChangeSummary)
		assert.Equal(t, []string{"docs/here.md"}, result.CitedPaths)
	})

	t.Run("empty orgID is rejected without a call", func(t *testing.T) {
		t.Parallel()
		var calls int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()
		client := NewLLMClient(server.URL, nil, nil)
		_, err := client.GenerateHarnessFix(context.Background(), "", "src-1", HarnessFixInput{})
		require.Error(t, err)
		assert.Equal(t, 0, calls)
	})

	t.Run("persistent 502 retries then returns an error", func(t *testing.T) {
		t.Parallel()
		var calls int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			http.Error(w, `{"detail":"generate_harness_fix output invalid: ..."}`, http.StatusBadGateway)
		}))
		defer server.Close()
		client := NewLLMClient(server.URL, nil, nil)
		client.retryBaseBackoff = 0 // no real sleeps in the test

		_, err := client.GenerateHarnessFix(context.Background(), "org-1", "src-1",
			HarnessFixInput{Dimension: "CONFLICT / broken-path", Title: "x", ReferencePoint: "y", CurrentContent: "z"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "HTTP 502")
		// A 502 is KindServer → retryable → retried up to the attempt cap.
		assert.Equal(t, llmMaxAttempts, calls, "a 502 must be retried up to the attempt cap")
	})

	t.Run("200 with non-JSON result surfaces as a parse error", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			resp := pyclient.GenerateContentResponse{Result: "not json", TaskType: pyclient.TaskTypeGenerateHarnessFix}
			assert.NoError(t, json.NewEncoder(w).Encode(resp))
		}))
		defer server.Close()
		client := NewLLMClient(server.URL, nil, nil)

		_, err := client.GenerateHarnessFix(context.Background(), "org-1", "src-1",
			HarnessFixInput{Dimension: "CONFLICT / broken-path", Title: "x", ReferencePoint: "y", CurrentContent: "z"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unmarshalling harness-fix result")
	})
}

// TestClassifyReviewThemes_SBERTNoEmitCost proves the SBERT path emits provider=ml
// with no usage/cost (token-free local model).
func TestClassifyReviewThemes_SBERTNoEmitCost(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req pyclient.ClassifyThemesRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		resp := pyclient.ClassifyThemesResponse{StrategyUsed: "sbert", Provider: "ml", Model: "all-MiniLM-L6-v2"}
		for localIndex := range req.Comments {
			resp.Classifications = append(resp.Classifications, pyclient.ThemeClassification{Index: localIndex, Theme: "testing", Confidence: 0.9})
		}
		assert.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
	defer server.Close()

	ctrl := gomock.NewController(t)
	emitter := llmanalyticsmocks.NewMockModelAnalyticsEmitter(ctrl)
	var got llmanalytics.ModelCallEvent
	emitter.EXPECT().EmitModelCall(gomock.Any(), gomock.Any()).
		Do(func(_ context.Context, ev llmanalytics.ModelCallEvent) { got = ev }).Times(1)

	client := NewLLMClient(server.URL, nil, nil, WithEmitter(emitter))
	_, err := client.ClassifyReviewThemes(context.Background(), "org-1", []string{"a", "b"}, 1)
	require.NoError(t, err)

	assert.Equal(t, "ml", got.Provider)
	assert.Equal(t, "all-MiniLM-L6-v2", got.Model)
	assert.Nil(t, got.Usage)
	assert.Nil(t, got.CostUSD)
}
