// Package service implements the AdminService and AgentService handlers over
// the store, the blob store and the pure pkg/spec, pkg/status and pkg/audience
// rules.
package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1"
	"github.com/tetriz-ai/tetriz-harness/pkg/audience"
	"github.com/tetriz-ai/tetriz-harness/pkg/spec"
	"github.com/tetriz-ai/tetriz-harness/server/blob"
	"github.com/tetriz-ai/tetriz-harness/server/store"
)

// Request bounds the proto leaves to the server.
const (
	MaxNameLen        = 200
	MaxBodyBytes      = 1 << 20
	MaxFiles          = 100
	MaxHashesPerCall  = 100
	MaxArtifacts      = 100
	MaxReasonLen      = 1000
	MaxInstalled      = 10000
	MaxKnownRepos     = 1000
	MaxFailures       = 1000
	MaxAttributeLen   = 200
	MaxSourcePathLen  = 1024
	MaxRepositoryLen  = 200
	EnforcementWindow = 15 * 24 * time.Hour
	// MaxEnforcementWindow bounds a caller-chosen enforce_until.
	MaxEnforcementWindow = 90 * 24 * time.Hour
)

var (
	errHooksDisabled   = errors.New("hook artifacts are disabled on this server; set HARNESS_ALLOW_HOOKS=true to allow them")
	errHookConfigNeed  = errors.New("hook_config is required for a HOOK artifact")
	errHookConfigExtra = errors.New("hook_config is allowed only for a HOOK artifact")
	errContentRequired = errors.New("inline_content or files is required when creating an artifact")
	errContentBoth     = errors.New("provide either inline_content or files, not both")
	errIdentityChange  = errors.New("ide_type and type cannot change on update")
	errRepoRequired    = errors.New("repository is required for a REPO_LOCAL placement")
	errRepoForbidden   = errors.New("repository must be empty for a USER_GLOBAL placement")
	errRepoInvalid     = errors.New(`repository must be "owner/repo"`)
	errInvalidID       = errors.New("id is not a valid uuid")
	errNoRepoLocal     = errors.New("artifact has no REPO_LOCAL placement")
	errPaused          = errors.New("enforcement is paused")
	errNotEnforced     = errors.New("content hash is not in the caller's enforced set")
)

// Store is the persistence the handlers need; *store.Store implements it.
// People and teams are not stored here: they come from a Directory.
type Store interface {
	CreateArtifact(ctx context.Context, a store.Artifact, files []store.ArtifactFile, blobs []store.Blob, maxLive int) (*store.Artifact, error)
	UpdateArtifact(ctx context.Context, a store.Artifact, files []store.ArtifactFile, blobs []store.Blob) (*store.Artifact, error)
	GetArtifact(ctx context.Context, id string, includeRetracted bool) (*store.Artifact, error)
	ListLiveArtifacts(ctx context.Context) ([]*store.Artifact, error)
	ListRetractedArtifacts(ctx context.Context) ([]*store.Artifact, error)
	ListArtifactFiles(ctx context.Context, artifactIDs []string) (map[string][]store.ArtifactFile, error)
	ListLiveTargets(ctx context.Context, artifactIDs []string) (map[string][]audience.Row, error)
	ListRetractedTargets(ctx context.Context, artifactIDs []string) (map[string][]audience.Row, error)
	SetTargets(ctx context.Context, artifactID string, scope v1.ArtifactScope, repository string, desired []audience.Row, enforceUntil time.Time) (time.Time, error)
	RetractArtifact(ctx context.Context, id string) error
	Paused(ctx context.Context) (bool, error)
	SetPaused(ctx context.Context, paused bool) error

	ReplaceReport(ctx context.Context, r store.Report) error
	ReportSummaries(ctx context.Context, personIDs []string) (map[string]*store.ReportSummary, error)
	Observations(ctx context.Context, keys []store.ObservationKey) (map[store.ObservationKey]*store.Observation, error)
	Failures(ctx context.Context, keys []store.FailureKey) (map[store.FailureKey]*store.Failure, error)
}

// Config is the handler behaviour the operator controls.
type Config struct {
	// AllowHooks admits HOOK artifacts, which execute code on every targeted machine.
	AllowHooks bool
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Version is the server build reported by GetSettings.
	Version string
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

var (
	uuidPattern       = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

func validateID(id string) error {
	if !uuidPattern.MatchString(id) {
		return invalid(fmt.Errorf("%q: %w", id, errInvalidID))
	}
	return nil
}

// normalizeRepository folds an "owner/repo" to lowercase: hosts treat the
// name case-insensitively, so targets and reports must meet in one spelling.
func normalizeRepository(repo string) string { return strings.ToLower(repo) }

func validateRepository(repo string) error {
	if len(repo) > MaxRepositoryLen || !repositoryPattern.MatchString(repo) {
		return invalid(fmt.Errorf("%q: %w", repo, errRepoInvalid))
	}
	return nil
}

func invalid(err error) error { return connect.NewError(connect.CodeInvalidArgument, err) }

func notFound(err error) error { return connect.NewError(connect.CodeNotFound, err) }

// toConnect maps domain errors to their wire code; anything unrecognised is
// Internal, whose detail the server logs and strips.
func toConnect(err error) error {
	var ce *connect.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ce):
		return err
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("not found"))
	case errors.Is(err, store.ErrSlugExists):
		return connect.NewError(connect.CodeAlreadyExists, store.ErrSlugExists)
	case errors.Is(err, store.ErrRootDocExists):
		return connect.NewError(connect.CodeAlreadyExists, store.ErrRootDocExists)
	case errors.Is(err, store.ErrTooManyArtifacts):
		return connect.NewError(connect.CodeResourceExhausted, store.ErrTooManyArtifacts)
	case errors.Is(err, spec.ErrTypeNotEnforceable), errors.Is(err, spec.ErrScopeNotSupported),
		errors.Is(err, spec.ErrIdeNotSupported), errors.Is(err, spec.ErrInvalidSlug),
		errors.Is(err, spec.ErrInvalidRelativePath), errors.Is(err, spec.ErrInvalidHookEvent),
		errors.Is(err, spec.ErrMultiFileTypeNotAllowed), errors.Is(err, spec.ErrPrimaryFileMissing),
		errors.Is(err, spec.ErrDuplicateRelativePath), errors.Is(err, audience.ErrInvalid),
		errors.Is(err, blob.ErrInvalidHash):
		return connect.NewError(connect.CodeInvalidArgument, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}
