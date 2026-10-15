// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package pyclient is a small Go HTTP client to the tetriz-py LLM service. It is pure transport
// (marshal → POST → size-capped read → status/typed-error → decode), with a post-success hook seam
// for analytics. Construction has no side effects; it registers nothing at import.
package pyclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/logger"
)

// Endpoint paths on the tetriz-py service.
const (
	generateContentAPI = "/api/v1/generate-content"
	classifyThemesAPI  = "/api/v1/classify-themes"
)

// Task-type identifiers, mirroring tetriz-py's task registry keys; a caller sets one on a request's
// TaskType. TaskTypeClassifyThemes labels only the caller's own analytics (classify-themes carries
// no request task_type).
const (
	TaskTypeDeriveOrgArtifacts    = "derive_org_artifacts"
	TaskTypeClassifyThemes        = "classify_themes"
	TaskTypeClassifyHarnessIntent = "classify_harness_intent"
	TaskTypeGenerateHarnessFix    = "generate_harness_fix"
)

// Transport size limits.
const (
	// DefaultMaxBody is the 10 MB response-body ceiling.
	DefaultMaxBody = 10 << 20
	// MaxErrBodyInMsg bounds how much response body is echoed into an error message.
	MaxErrBodyInMsg = 512
)

// defaultHTTPTimeout is a last-resort backstop so a caller that sets no context deadline fails
// bounded rather than hanging forever.
const defaultHTTPTimeout = 10 * time.Minute

// SuccessHook is invoked after a successful GenerateContent call so a caller can emit analytics.
// ClassifyThemes deliberately has no hook (its caller aggregates usage across batches and emits once).
type SuccessHook interface {
	OnGenerateContent(ctx context.Context, req GenerateContentRequest, resp *GenerateContentResponse)
}

// Client is the tetriz-py HTTP client; construct it with New.
type Client struct {
	baseURL    string
	httpClient *http.Client
	hook       SuccessHook   // nil = no emission
	timeout    time.Duration // per-call cap via context deadline; 0 = only the http.Client backstop
}

// Option configures a Client at construction.
type Option func(*Client)

// WithSuccessHook attaches the post-success emit seam (nil disables emission).
func WithSuccessHook(h SuccessHook) Option { return func(c *Client) { c.hook = h } }

// New builds a client pointing at the tetriz-py base URL.
func New(baseURL string, opts ...Option) *Client {
	c := &Client{baseURL: baseURL, httpClient: &http.Client{Timeout: defaultHTTPTimeout}}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// TokenUsage is the token breakdown a tetriz-py response carries.
type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	ThinkingTokens   int `json:"thoughts_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
}

// GenerateContentRequest is the shared generate-content request.
type GenerateContentRequest struct {
	Content              string         `json:"content"`
	TaskType             string         `json:"task_type"`
	OrgID                string         `json:"org_id"`
	SourceID             string         `json:"source_id"`
	SessionContext       map[string]any `json:"session_context,omitempty"`
	UseSystemPromptCache bool           `json:"use_system_prompt_cache"`
}

// GenerateContentResponse is the common generate-content envelope; Result is the task-specific JSON
// the caller parses, Raw the verbatim body.
type GenerateContentResponse struct {
	Result   string          `json:"result"`
	TaskType string          `json:"task_type"`
	Model    string          `json:"model"`
	Provider string          `json:"provider"`
	Usage    *TokenUsage     `json:"usage,omitempty"`
	CostUSD  *float64        `json:"cost_usd,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
	Raw      []byte          `json:"-"`
}

// GenerateContent POSTs the request, returns the decoded envelope, and fires the SuccessHook on success.
func (c *Client) GenerateContent(ctx context.Context, req GenerateContentRequest) (*GenerateContentResponse, error) {
	var response GenerateContentResponse
	body, err := c.postJSON(ctx, generateContentAPI, req, &response, DefaultMaxBody)
	if err != nil {
		return nil, fmt.Errorf("generate-content: %w", err)
	}
	response.Raw = body
	if c.hook != nil {
		c.hook.OnGenerateContent(ctx, req, &response)
	}
	return &response, nil
}

// ClassifyThemesRequest is one classify-themes batch.
type ClassifyThemesRequest struct {
	OrgID               string   `json:"org_id"`
	Comments            []string `json:"comments"`
	MaxThemesPerComment int      `json:"max_themes_per_comment,omitempty"`
}

// ThemeClassification is one classified comment; Index is the batch-local position.
type ThemeClassification struct {
	Index      int     `json:"index"`
	Theme      string  `json:"theme"`
	Confidence float64 `json:"confidence"`
}

// ClassifyThemesResponse is the classify-themes response.
type ClassifyThemesResponse struct {
	Classifications []ThemeClassification `json:"classifications"`
	StrategyUsed    string                `json:"strategy_used"`
	Provider        string                `json:"provider"`
	Model           string                `json:"model"`
	Usage           *TokenUsage           `json:"usage,omitempty"`
	CostUSD         *float64              `json:"cost_usd,omitempty"`
}

// ClassifyThemes POSTs one batch. It fires no SuccessHook — the caller aggregates usage across
// batches and emits once itself.
func (c *Client) ClassifyThemes(ctx context.Context, req ClassifyThemesRequest) (*ClassifyThemesResponse, error) {
	var response ClassifyThemesResponse
	if _, err := c.postJSON(ctx, classifyThemesAPI, req, &response, DefaultMaxBody); err != nil {
		return nil, fmt.Errorf("classify-themes: %w", err)
	}
	return &response, nil
}

// postJSON is the transport primitive every endpoint method calls.
func (c *Client) postJSON(ctx context.Context, path string, req, responseTarget any, maxBody int64) ([]byte, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, &Error{Kind: KindRequestBuild, err: fmt.Errorf("marshal request: %w", err)}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(reqBytes))
	if err != nil {
		return nil, &Error{Kind: KindRequestBuild, err: fmt.Errorf("build request: %w", err)}
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, &Error{Kind: classifyTransport(err), err: fmt.Errorf("do request: %w", err)}
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			logger.WarnWithCtx(ctx, "pyclient: closing response body failed", zap.Error(cerr))
		}
	}()

	// +1 so a body that hits the cap surfaces as an explicit KindResponseTooLarge rather than a
	// silent truncation.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, &Error{Kind: KindTransport, Status: resp.StatusCode, err: fmt.Errorf("read response body: %w", err)}
	}
	if int64(len(body)) > maxBody {
		return body, &Error{
			Kind: KindResponseTooLarge, Status: resp.StatusCode, Body: body,
			err: fmt.Errorf("response body exceeds %d bytes", maxBody),
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, &Error{
			Kind: classifyBody(classifyStatus(resp.StatusCode), body), Status: resp.StatusCode, Body: body,
			err: fmt.Errorf("tetriz-py returned HTTP %d: %s", resp.StatusCode, Truncate(string(body), MaxErrBodyInMsg)),
		}
	}
	if responseTarget != nil {
		if err := json.Unmarshal(body, responseTarget); err != nil {
			return body, &Error{Kind: KindParse, Status: resp.StatusCode, Body: body, err: fmt.Errorf("unmarshal response: %w", err)}
		}
	}
	return body, nil
}

// ErrorKind classifies why a tetriz-py call failed.
type ErrorKind int

const (
	KindTimeout ErrorKind = iota
	KindCanceled
	KindTransport
	KindTooManyRequests
	KindServer
	KindClient
	KindParse
	KindResponseTooLarge
	KindRequestBuild
	// KindOutputTruncated is a 5xx whose body identifies itself as a model output-budget truncation.
	// Separated from KindServer because it is DETERMINISTIC — the same request truncates again — so a
	// caller that retries a 5xx by default must stop on this one.
	KindOutputTruncated
)

// String renders the kind as a stable, readable token for operator logs.
func (k ErrorKind) String() string {
	switch k {
	case KindTimeout:
		return "timeout"
	case KindCanceled:
		return "canceled"
	case KindTransport:
		return "transport"
	case KindTooManyRequests:
		return "too_many_requests"
	case KindServer:
		return "server"
	case KindClient:
		return "client"
	case KindParse:
		return "parse"
	case KindResponseTooLarge:
		return "response_too_large"
	case KindRequestBuild:
		return "request_build"
	case KindOutputTruncated:
		return "output_truncated"
	default:
		return fmt.Sprintf("unknown(%d)", int(k))
	}
}

// Error is the single typed error the client returns. Body holds the full response bytes (bounded by
// the endpoint ceiling); Status is the HTTP status, or 0 when no response arrived.
type Error struct {
	Kind   ErrorKind
	Status int
	Body   []byte
	err    error
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("pyclient: kind=%s status=%d: %v", e.Kind, e.Status, e.err)
	}
	return fmt.Sprintf("pyclient: kind=%s: %v", e.Kind, e.err)
}

func (e *Error) Unwrap() error { return e.err }

func classifyTransport(err error) ErrorKind {
	switch {
	case errors.Is(err, context.Canceled):
		return KindCanceled
	case isTimeout(err):
		return KindTimeout
	default:
		return KindTransport
	}
}

func classifyStatus(code int) ErrorKind {
	switch {
	case code >= http.StatusInternalServerError:
		return KindServer
	case code == http.StatusTooManyRequests:
		return KindTooManyRequests
	default:
		return KindClient
	}
}

// errorCodeOutputTruncated is the `code` tetriz-py sets on a model output-budget truncation; a wire
// contract shared with the service's exceptions.
const errorCodeOutputTruncated = "OUTPUT_TRUNCATED"

// classifyBody refines a status-derived kind using the response body's own `code` (the service
// answers 502 for every LLM failure, transient and permanent alike, so the status alone cannot tell
// a restarted pod from an output that truncates identically on retry). An unparseable or code-less
// body leaves the status-derived kind untouched.
func classifyBody(kind ErrorKind, body []byte) ErrorKind {
	if len(body) == 0 {
		return kind
	}
	var envelope struct {
		Detail struct {
			Code string `json:"code"`
		} `json:"detail"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return kind
	}
	code := envelope.Detail.Code
	if code == "" {
		code = envelope.Code
	}
	if code == errorCodeOutputTruncated {
		return KindOutputTruncated
	}
	return kind
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// Truncate caps s at maxLen runes (never slicing mid-rune) without allocating the whole string as runes.
func Truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if limit := maxLen * utf8.UTFMax; limit < len(s) {
		s = s[:limit]
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return string(runes)
	}
	return string(runes[:maxLen])
}
