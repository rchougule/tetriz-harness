package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1"
	"github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1/harnessctlv1connect"
	"github.com/tetriz-ai/tetriz-harness/pkg/audience"
	"github.com/tetriz-ai/tetriz-harness/pkg/spec"
	"github.com/tetriz-ai/tetriz-harness/pkg/status"
	"github.com/tetriz-ai/tetriz-harness/server/blob"
	"github.com/tetriz-ai/tetriz-harness/server/store"
)

type personKey struct{}

// WithPerson binds the person the host authenticated to ctx; every agent
// call acts as that person.
func WithPerson(ctx context.Context, personID string) context.Context {
	return context.WithValue(ctx, personKey{}, personID)
}

// PersonFrom returns the person bound by WithPerson.
func PersonFrom(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(personKey{}).(string)
	return id, ok && id != ""
}

var errNoPerson = errors.New("no authenticated person")

// Agent implements AgentService; every call is scoped to the person the host
// bound with WithPerson.
type Agent struct {
	store Store
	dir   Directory
	blobs blob.Store
	cfg   Config
	log   *slog.Logger
}

var _ harnessctlv1connect.AgentServiceHandler = (*Agent)(nil)

// NewAgent builds the agent handlers over the host's directory.
func NewAgent(s Store, dir Directory, b blob.Store, cfg Config, log *slog.Logger) *Agent {
	if log == nil {
		log = slog.Default()
	}
	return &Agent{store: s, dir: dir, blobs: b, cfg: cfg, log: log}
}

func person(ctx context.Context) (string, error) {
	id, ok := PersonFrom(ctx)
	if !ok {
		return "", connect.NewError(connect.CodeUnauthenticated, errNoPerson)
	}
	return id, nil
}

// enforcedSet resolves the person's artifacts. When paused it returns no
// artifacts and paused=true, which the agent must not read as removal.
func (a *Agent) enforcedSet(ctx context.Context, personID string) ([]*v1.EnforcedArtifact, bool, error) {
	paused, err := a.store.Paused(ctx)
	if err != nil {
		return nil, false, err
	}
	if paused {
		return nil, true, nil
	}
	all, err := a.store.ListLiveArtifacts(ctx)
	if err != nil {
		return nil, false, err
	}
	// With hooks disabled a stored hook is not served either, so turning the
	// opt-in off stops execution rather than only new writes.
	defs := make([]*store.Artifact, 0, len(all))
	ids := make([]string, 0, len(all))
	for _, d := range all {
		if d.Type == v1.ArtifactType_ARTIFACT_TYPE_HOOK && !a.cfg.AllowHooks {
			continue
		}
		defs = append(defs, d)
		ids = append(ids, d.ID)
	}
	targets, err := a.store.ListLiveTargets(ctx, ids)
	if err != nil {
		return nil, false, err
	}
	files, err := a.store.ListArtifactFiles(ctx, ids)
	if err != nil {
		return nil, false, err
	}
	var teams map[string]bool
	teamsOf := func() (map[string]bool, error) {
		if teams != nil {
			return teams, nil
		}
		ids, err := a.dir.PersonTeams(ctx, personID)
		if err != nil {
			return nil, fmt.Errorf("listing teams of person: %w", err)
		}
		teams = make(map[string]bool, len(ids))
		for _, id := range ids {
			teams[id] = true
		}
		return teams, nil
	}
	now := a.cfg.now()
	var out []*v1.EnforcedArtifact
	for _, d := range defs {
		matched, err := audience.MatchPerson(targets[d.ID], personID, teamsOf)
		if err != nil {
			return nil, false, err
		}
		for _, row := range matched {
			installPath, err := spec.InstallPath(d.IdeType, d.Type, d.Slug, row.Scope)
			if err != nil {
				a.log.WarnContext(ctx, "skipping placement with no install path", "artifact_id", d.ID, "scope", row.Scope.String(), "error", err)
				continue
			}
			until := d.EnforceUntil
			ea := &v1.EnforcedArtifact{
				ArtifactId:   d.ID,
				IdeType:      d.IdeType,
				Type:         d.Type,
				Name:         d.Name,
				Scope:        row.Scope,
				Repository:   row.Repository,
				InstallPath:  installPath,
				PrimaryHash:  d.PrimaryHash,
				EnforceUntil: timestamppb.New(until),
				Expired:      status.IsExpired(&until, now),
			}
			isHook := d.Type == v1.ArtifactType_ARTIFACT_TYPE_HOOK
			for _, f := range files[d.ID] {
				ea.Files = append(ea.Files, &v1.EnforcedFile{
					RelativePath: spec.FileInstallPath(installPath, f.RelativePath),
					ContentHash:  f.ContentHash,
					Executable:   isHook && f.IsPrimary,
				})
			}
			if isHook {
				cmd, err := spec.HookCommand(d.IdeType, row.Scope, d.Slug)
				if err != nil {
					return nil, false, fmt.Errorf("deriving hook command for %s: %w", d.ID, err)
				}
				ea.HookConfig = d.HookConfig
				ea.HookCommand = cmd
			}
			out = append(out, ea)
		}
	}
	return out, false, nil
}

// GetEnforcedSet returns the caller's enforcement set without bodies.
func (a *Agent) GetEnforcedSet(ctx context.Context, _ *connect.Request[v1.GetEnforcedSetRequest]) (*connect.Response[v1.GetEnforcedSetResponse], error) {
	pid, err := person(ctx)
	if err != nil {
		return nil, err
	}
	arts, paused, err := a.enforcedSet(ctx, pid)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&v1.GetEnforcedSetResponse{Artifacts: arts, Paused: paused}), nil
}

// GetArtifactBodies returns bodies by hash, only for hashes in the caller's own set.
func (a *Agent) GetArtifactBodies(ctx context.Context, req *connect.Request[v1.GetArtifactBodiesRequest]) (*connect.Response[v1.GetArtifactBodiesResponse], error) {
	pid, err := person(ctx)
	if err != nil {
		return nil, err
	}
	hashes := req.Msg.GetContentHashes()
	if len(hashes) > MaxHashesPerCall {
		return nil, invalid(fmt.Errorf("at most %d content hashes per call", MaxHashesPerCall))
	}
	for _, h := range hashes {
		if err := blob.ValidateHash(h); err != nil {
			return nil, invalid(fmt.Errorf("%q: %w", h, err))
		}
	}
	arts, paused, err := a.enforcedSet(ctx, pid)
	if err != nil {
		return nil, toConnect(err)
	}
	if paused {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errPaused)
	}
	allowed := map[string]bool{}
	for _, ea := range arts {
		for _, f := range ea.GetFiles() {
			allowed[f.GetContentHash()] = true
		}
	}
	out := &v1.GetArtifactBodiesResponse{}
	seen := map[string]bool{}
	for _, h := range hashes {
		if seen[h] {
			continue
		}
		seen[h] = true
		if !allowed[h] {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s: %w", h, errNotEnforced))
		}
		content, err := a.blobs.Get(ctx, h)
		if err != nil {
			return nil, toConnect(fmt.Errorf("loading body %s: %w", h, err))
		}
		out.Bodies = append(out.Bodies, &v1.ArtifactBody{ContentHash: h, Content: content})
	}
	return connect.NewResponse(out), nil
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

func validateAttribute(field, v string) error {
	if len(v) > MaxAttributeLen {
		return invalid(fmt.Errorf("%s exceeds %d bytes", field, MaxAttributeLen))
	}
	return nil
}

func validIde(ide v1.IdeType) bool {
	_, known := v1.IdeType_name[int32(ide)]
	return known && ide != v1.IdeType_IDE_TYPE_UNSPECIFIED
}

// ReportState replaces the caller's report: the last report wins.
func (a *Agent) ReportState(ctx context.Context, req *connect.Request[v1.ReportStateRequest]) (*connect.Response[v1.ReportStateResponse], error) {
	pid, err := person(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if err := validateAttribute("device_id", m.GetDeviceId()); err != nil {
		return nil, err
	}
	if err := validateAttribute("agent_version", m.GetAgentVersion()); err != nil {
		return nil, err
	}
	if len(m.GetKnownRepositories()) > MaxKnownRepos {
		return nil, invalid(fmt.Errorf("at most %d known_repositories", MaxKnownRepos))
	}
	if len(m.GetInstalled()) > MaxInstalled {
		return nil, invalid(fmt.Errorf("at most %d installed entries", MaxInstalled))
	}
	if len(m.GetApplyFailures()) > MaxFailures {
		return nil, invalid(fmt.Errorf("at most %d apply_failures", MaxFailures))
	}
	r := store.Report{
		PersonID:     pid,
		DeviceID:     m.GetDeviceId(),
		AgentVersion: m.GetAgentVersion(),
		ReportedAt:   a.cfg.now(),
	}
	knownSeen := map[string]bool{}
	for _, repo := range m.GetKnownRepositories() {
		repo = normalizeRepository(repo)
		if err := validateRepository(repo); err != nil {
			return nil, err
		}
		if !knownSeen[repo] {
			knownSeen[repo] = true
			r.KnownRepositories = append(r.KnownRepositories, repo)
		}
	}
	type identity struct {
		ide                  v1.IdeType
		repo, checkout, path string
	}
	seen := map[identity]bool{}
	for _, in := range m.GetInstalled() {
		if !validIde(in.GetIdeType()) {
			return nil, invalid(fmt.Errorf("installed entry has unknown ide_type %v", in.GetIdeType()))
		}
		repo := normalizeRepository(in.GetRepository())
		if repo != "" {
			if err := validateRepository(repo); err != nil {
				return nil, err
			}
		}
		if in.GetCheckout() != "" && repo == "" {
			return nil, invalid(errors.New("installed checkout is set without a repository"))
		}
		if err := validateAttribute("installed checkout", in.GetCheckout()); err != nil {
			return nil, err
		}
		if in.GetSourcePath() == "" || len(in.GetSourcePath()) > MaxSourcePathLen {
			return nil, invalid(fmt.Errorf("installed source_path must be 1-%d bytes", MaxSourcePathLen))
		}
		if err := blob.ValidateHash(in.GetContentHash()); err != nil {
			return nil, invalid(fmt.Errorf("installed %q: %w", in.GetSourcePath(), err))
		}
		id := identity{in.GetIdeType(), repo, in.GetCheckout(), in.GetSourcePath()}
		if seen[id] {
			return nil, invalid(fmt.Errorf("installed entry %s %q %q %q is reported twice", in.GetIdeType(), repo, in.GetCheckout(), in.GetSourcePath()))
		}
		seen[id] = true
		r.Installed = append(r.Installed, store.Installed{
			IdeType: in.GetIdeType(), Repository: repo, Checkout: in.GetCheckout(), SourcePath: in.GetSourcePath(), ContentHash: in.GetContentHash(),
		})
	}
	for _, f := range m.GetApplyFailures() {
		if err := validateID(f.GetArtifactId()); err != nil {
			return nil, err
		}
		repo := normalizeRepository(f.GetRepository())
		if repo != "" {
			if err := validateRepository(repo); err != nil {
				return nil, err
			}
		}
		if f.GetContentHash() != "" {
			if err := blob.ValidateHash(f.GetContentHash()); err != nil {
				return nil, invalid(fmt.Errorf("apply failure: %w", err))
			}
		}
		r.Failures = append(r.Failures, store.ApplyFailure{
			ArtifactID: f.GetArtifactId(), Repository: repo, ContentHash: f.GetContentHash(),
			Reason: truncateRunes(f.GetReason(), MaxReasonLen),
		})
	}
	if err := a.store.ReplaceReport(ctx, r); err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&v1.ReportStateResponse{}), nil
}
