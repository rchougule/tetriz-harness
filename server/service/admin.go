package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1"
	"github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1/harnessctlv1connect"
	"github.com/tetriz-ai/tetriz-harness/pkg/audience"
	"github.com/tetriz-ai/tetriz-harness/pkg/spec"
	"github.com/tetriz-ai/tetriz-harness/pkg/status"
	"github.com/tetriz-ai/tetriz-harness/server/blob"
	"github.com/tetriz-ai/tetriz-harness/server/store"
)

// Admin implements AdminService.
type Admin struct {
	store Store
	dir   Directory
	blobs blob.Store
	cfg   Config
}

var _ harnessctlv1connect.AdminServiceHandler = (*Admin)(nil)

// NewAdmin builds the admin handlers over the host's directory.
func NewAdmin(s Store, dir Directory, b blob.Store, cfg Config) *Admin {
	return &Admin{store: s, dir: dir, blobs: b, cfg: cfg}
}

// validateKind applies the type gate: HOOK needs the operator opt-in and a
// hook_config in the tool's vocabulary; every other type must be an
// instruction type and carry no hook_config.
func (a *Admin) validateKind(ide v1.IdeType, t v1.ArtifactType, hc *v1.HookConfig) error {
	if t == v1.ArtifactType_ARTIFACT_TYPE_HOOK {
		if !a.cfg.AllowHooks {
			return connect.NewError(connect.CodeFailedPrecondition, errHooksDisabled)
		}
		if hc == nil {
			return invalid(errHookConfigNeed)
		}
		if err := spec.ValidateHookEvent(ide, hc.GetEvent()); err != nil {
			return invalid(err)
		}
		if hc.GetTimeoutSeconds() < 0 {
			return invalid(fmt.Errorf("hook_config.timeout_seconds must not be negative"))
		}
		return nil
	}
	if hc != nil {
		return invalid(errHookConfigExtra)
	}
	if !spec.IsInstructionType(t) {
		return invalid(spec.ErrTypeNotEnforceable)
	}
	return nil
}

// primaryRoot derives the primary's path at the first scope the definition can
// be placed at, which fixes the primary's file name for a file set.
func primaryRoot(ide v1.IdeType, t v1.ArtifactType, slug string) (string, error) {
	var firstErr error
	for _, scope := range []v1.ArtifactScope{v1.ArtifactScope_ARTIFACT_SCOPE_USER_GLOBAL, v1.ArtifactScope_ARTIFACT_SCOPE_REPO_LOCAL} {
		p, err := spec.InstallPath(ide, t, slug, scope)
		if err == nil {
			return p, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return "", firstErr
}

type body struct {
	files []store.ArtifactFile
	blobs []store.Blob
	data  map[string][]byte
}

func (b *body) add(rel string, primary bool, content []byte) error {
	if len(content) > MaxBodyBytes {
		return invalid(fmt.Errorf("file %q exceeds %d bytes", rel, MaxBodyBytes))
	}
	h := blob.Hash(content)
	b.files = append(b.files, store.ArtifactFile{RelativePath: rel, ContentHash: h, IsPrimary: primary})
	if _, ok := b.data[h]; !ok {
		b.data[h] = content
		b.blobs = append(b.blobs, store.Blob{ContentHash: h, Size: int64(len(content))})
	}
	return nil
}

func (b *body) primaryHash() string {
	for _, f := range b.files {
		if f.IsPrimary {
			return f.ContentHash
		}
	}
	return ""
}

// resolveBody validates the request's content; nil means the request carries none.
func resolveBody(req *v1.UpsertArtifactRequest, root string) (*body, error) {
	hasInline := req.GetInlineContent() != ""
	hasFiles := len(req.GetFiles()) > 0
	switch {
	case hasInline && hasFiles:
		return nil, invalid(errContentBoth)
	case hasInline:
		b := &body{data: map[string][]byte{}}
		if err := b.add("", true, []byte(req.GetInlineContent())); err != nil {
			return nil, err
		}
		return b, nil
	case hasFiles:
		if len(req.GetFiles()) > MaxFiles {
			return nil, invalid(fmt.Errorf("at most %d files are allowed", MaxFiles))
		}
		rels := make([]string, 0, len(req.GetFiles()))
		for _, f := range req.GetFiles() {
			rels = append(rels, f.GetRelativePath())
		}
		primary, err := spec.ValidateFileSet(req.GetType(), root, rels)
		if err != nil {
			return nil, invalid(err)
		}
		b := &body{data: map[string][]byte{}}
		for i, f := range req.GetFiles() {
			rel := f.GetRelativePath()
			if i == primary {
				rel = ""
			}
			if err := b.add(rel, i == primary, []byte(f.GetContent())); err != nil {
				return nil, err
			}
		}
		return b, nil
	default:
		return nil, nil
	}
}

func (a *Admin) putBlobs(ctx context.Context, b *body) error {
	for _, bl := range b.blobs {
		if err := a.blobs.Put(ctx, bl.ContentHash, b.data[bl.ContentHash]); err != nil {
			return fmt.Errorf("storing body: %w", err)
		}
	}
	return nil
}

// enforceUntil resolves a requested window close: unset means the default
// window from now; a set one must lie in (now, now+MaxEnforcementWindow].
func (a *Admin) enforceUntil(req *timestamppb.Timestamp) (time.Time, error) {
	now := a.cfg.now()
	if req == nil {
		return now.Add(EnforcementWindow), nil
	}
	if err := req.CheckValid(); err != nil {
		return time.Time{}, invalid(fmt.Errorf("enforce_until: %w", err))
	}
	until := req.AsTime()
	if !until.After(now) {
		return time.Time{}, invalid(errors.New("enforce_until must be in the future"))
	}
	if until.After(now.Add(MaxEnforcementWindow)) {
		return time.Time{}, invalid(fmt.Errorf("enforce_until must be at most %d days ahead", int(MaxEnforcementWindow.Hours()/24)))
	}
	return until, nil
}

// UpsertArtifact creates or updates a definition; every save restarts the window.
func (a *Admin) UpsertArtifact(ctx context.Context, req *connect.Request[v1.UpsertArtifactRequest]) (*connect.Response[v1.UpsertArtifactResponse], error) {
	m := req.Msg
	if err := a.validateKind(m.GetIdeType(), m.GetType(), m.GetHookConfig()); err != nil {
		return nil, err
	}
	if err := spec.ValidateSlug(m.GetSlug()); err != nil {
		return nil, invalid(err)
	}
	if len(m.GetName()) > MaxNameLen {
		return nil, invalid(fmt.Errorf("name exceeds %d bytes", MaxNameLen))
	}
	until, err := a.enforceUntil(m.GetEnforceUntil())
	if err != nil {
		return nil, err
	}
	root, err := primaryRoot(m.GetIdeType(), m.GetType(), m.GetSlug())
	if err != nil {
		return nil, invalid(err)
	}
	b, err := resolveBody(m, root)
	if err != nil {
		return nil, err
	}

	var existing *store.Artifact
	if m.GetArtifactId() != "" {
		if err := validateID(m.GetArtifactId()); err != nil {
			return nil, err
		}
		existing, err = a.store.GetArtifact(ctx, m.GetArtifactId(), false)
		if err != nil {
			return nil, toConnect(err)
		}
		if existing.IdeType != m.GetIdeType() || existing.Type != m.GetType() {
			return nil, invalid(errIdentityChange)
		}
	} else if b == nil {
		return nil, invalid(errContentRequired)
	}

	// Bodies land before the rows that reference them, so a committed
	// definition never points at a missing body.
	if b != nil {
		if err := a.putBlobs(ctx, b); err != nil {
			return nil, toConnect(err)
		}
	}
	def := store.Artifact{
		IdeType:      m.GetIdeType(),
		Type:         m.GetType(),
		Slug:         m.GetSlug(),
		Name:         m.GetName(),
		HookConfig:   m.GetHookConfig(),
		EnforceUntil: until,
	}
	var saved *store.Artifact
	if existing != nil {
		def.ID = existing.ID
		var files []store.ArtifactFile
		var blobs []store.Blob
		if b != nil {
			files, blobs, def.PrimaryHash = b.files, b.blobs, b.primaryHash()
		}
		saved, err = a.store.UpdateArtifact(ctx, def, files, blobs)
	} else {
		def.PrimaryHash = b.primaryHash()
		saved, err = a.store.CreateArtifact(ctx, def, b.files, b.blobs, MaxArtifacts)
	}
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&v1.UpsertArtifactResponse{
		ArtifactId:   saved.ID,
		EnforceUntil: timestamppb.New(saved.EnforceUntil),
	}), nil
}

func placementsProto(rows []audience.Row) []*v1.Placement {
	groups := audience.GroupByPlacement(rows)
	out := make([]*v1.Placement, 0, len(groups))
	for _, g := range groups {
		aud := g.Audience()
		out = append(out, &v1.Placement{
			Scope:      g.Scope,
			Repository: g.Repository,
			Audience:   &v1.Audience{OrgWide: aud.OrgWide, PersonIds: aud.PersonIDs, TeamIds: aud.TeamIDs},
		})
	}
	return out
}

// summarize builds each definition's listing entry, status counts included,
// grading all of them in one pass.
func (a *Admin) summarize(ctx context.Context, defs []*store.Artifact) ([]*v1.ArtifactSummary, error) {
	targets, err := a.targetsOf(ctx, defs)
	if err != nil {
		return nil, err
	}
	graded, err := a.grade(ctx, defs, targets)
	if err != nil {
		return nil, err
	}
	out := make([]*v1.ArtifactSummary, 0, len(defs))
	for _, d := range defs {
		out = append(out, &v1.ArtifactSummary{
			ArtifactId:   d.ID,
			IdeType:      d.IdeType,
			Type:         d.Type,
			Slug:         d.Slug,
			Name:         d.Name,
			PrimaryHash:  d.PrimaryHash,
			HookConfig:   d.HookConfig,
			Placements:   placementsProto(targets[d.ID]),
			UpdatedAt:    timestamppb.New(d.UpdatedAt),
			EnforceUntil: timestamppb.New(d.EnforceUntil),
			StatusCounts: countStatuses(graded[d.ID]),
		})
		if d.DeletedAt != nil {
			out[len(out)-1].RetractedAt = timestamppb.New(*d.DeletedAt)
		}
	}
	return out, nil
}

// ListArtifacts returns every live definition, then retracted ones on request.
func (a *Admin) ListArtifacts(ctx context.Context, req *connect.Request[v1.ListArtifactsRequest]) (*connect.Response[v1.ListArtifactsResponse], error) {
	defs, err := a.store.ListLiveArtifacts(ctx)
	if err != nil {
		return nil, toConnect(err)
	}
	if req.Msg.GetIncludeRetracted() {
		retracted, err := a.store.ListRetractedArtifacts(ctx)
		if err != nil {
			return nil, toConnect(err)
		}
		defs = append(defs, retracted...)
	}
	sums, err := a.summarize(ctx, defs)
	if err != nil {
		return nil, toConnect(err)
	}
	paused, err := a.store.Paused(ctx)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&v1.ListArtifactsResponse{EnforcementPaused: paused, Artifacts: sums}), nil
}

// GetArtifact returns one definition, live or retracted, with its stored bodies.
func (a *Admin) GetArtifact(ctx context.Context, req *connect.Request[v1.GetArtifactRequest]) (*connect.Response[v1.GetArtifactResponse], error) {
	if err := validateID(req.Msg.GetArtifactId()); err != nil {
		return nil, err
	}
	def, err := a.store.GetArtifact(ctx, req.Msg.GetArtifactId(), true)
	if err != nil {
		return nil, toConnect(err)
	}
	sums, err := a.summarize(ctx, []*store.Artifact{def})
	if err != nil {
		return nil, toConnect(err)
	}
	files, err := a.store.ListArtifactFiles(ctx, []string{def.ID})
	if err != nil {
		return nil, toConnect(err)
	}
	out := &v1.GetArtifactResponse{Artifact: sums[0]}
	primaryName := ""
	if def.Type == v1.ArtifactType_ARTIFACT_TYPE_SKILL {
		root, err := primaryRoot(def.IdeType, def.Type, def.Slug)
		if err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		primaryName = spec.PrimaryFileName(root)
	}
	for _, f := range files[def.ID] {
		content, err := a.blobs.Get(ctx, f.ContentHash)
		if err != nil {
			return nil, toConnect(fmt.Errorf("loading body %s: %w", f.ContentHash, err))
		}
		if f.IsPrimary {
			out.InlineContent = string(content)
		}
		if primaryName == "" {
			continue
		}
		rel := f.RelativePath
		if f.IsPrimary {
			rel = primaryName
		}
		out.Files = append(out.Files, &v1.ArtifactFile{RelativePath: rel, Content: string(content)})
	}
	return connect.NewResponse(out), nil
}

// RemoveArtifact retracts a definition; agents delete the files they placed for it.
func (a *Admin) RemoveArtifact(ctx context.Context, req *connect.Request[v1.RemoveArtifactRequest]) (*connect.Response[v1.RemoveArtifactResponse], error) {
	if err := validateID(req.Msg.GetArtifactId()); err != nil {
		return nil, err
	}
	if err := a.store.RetractArtifact(ctx, req.Msg.GetArtifactId()); err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&v1.RemoveArtifactResponse{}), nil
}

// SetTargets replaces one placement's audience and restarts the window without shortening it.
func (a *Admin) SetTargets(ctx context.Context, req *connect.Request[v1.SetTargetsRequest]) (*connect.Response[v1.SetTargetsResponse], error) {
	m := req.Msg
	repo := normalizeRepository(m.GetRepository())
	if err := validateID(m.GetArtifactId()); err != nil {
		return nil, err
	}
	switch m.GetScope() {
	case v1.ArtifactScope_ARTIFACT_SCOPE_USER_GLOBAL:
		if repo != "" {
			return nil, invalid(errRepoForbidden)
		}
	case v1.ArtifactScope_ARTIFACT_SCOPE_REPO_LOCAL:
		if repo == "" {
			return nil, invalid(errRepoRequired)
		}
		if err := validateRepository(repo); err != nil {
			return nil, err
		}
	default:
		return nil, invalid(spec.ErrScopeNotSupported)
	}
	aud, err := audience.Resolve(m.GetAudience().GetOrgWide(), m.GetAudience().GetPersonIds(), m.GetAudience().GetTeamIds())
	if err != nil {
		return nil, invalid(err)
	}
	for _, id := range append(append([]string{}, aud.PersonIDs...), aud.TeamIDs...) {
		if err := validateHostID(id); err != nil {
			return nil, err
		}
	}
	def, err := a.store.GetArtifact(ctx, m.GetArtifactId(), false)
	if err != nil {
		return nil, toConnect(err)
	}
	if def.Type == v1.ArtifactType_ARTIFACT_TYPE_HOOK && !a.cfg.AllowHooks {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errHooksDisabled)
	}
	installPath, err := spec.InstallPath(def.IdeType, def.Type, def.Slug, m.GetScope())
	if err != nil {
		return nil, invalid(err)
	}
	if err := requireKnown(ctx, a.dir, aud.PersonIDs, aud.TeamIDs); err != nil {
		return nil, toConnect(err)
	}
	until, err := a.store.SetTargets(ctx, def.ID, m.GetScope(), repo, aud.Rows(m.GetScope(), repo), a.cfg.now().Add(EnforcementWindow))
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&v1.SetTargetsResponse{InstallPath: installPath, EnforceUntil: timestamppb.New(until)}), nil
}

// GetEnforcementStatus computes a definition's status per targeted person per
// placement. A retracted definition is graded against the audience its
// retraction removed.
func (a *Admin) GetEnforcementStatus(ctx context.Context, req *connect.Request[v1.GetEnforcementStatusRequest]) (*connect.Response[v1.GetEnforcementStatusResponse], error) {
	if err := validateID(req.Msg.GetArtifactId()); err != nil {
		return nil, err
	}
	def, err := a.store.GetArtifact(ctx, req.Msg.GetArtifactId(), true)
	if err != nil {
		return nil, toConnect(err)
	}
	targets, err := a.targetsOf(ctx, []*store.Artifact{def})
	if err != nil {
		return nil, toConnect(err)
	}
	graded, err := a.grade(ctx, []*store.Artifact{def}, targets)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&v1.GetEnforcementStatusResponse{Statuses: graded[def.ID]}), nil
}

// targetsOf loads each definition's routing rows: live rows for a live one,
// the rows its retraction removed for a retracted one.
func (a *Admin) targetsOf(ctx context.Context, defs []*store.Artifact) (map[string][]audience.Row, error) {
	var liveIDs, retractedIDs []string
	for _, d := range defs {
		if d.DeletedAt != nil {
			retractedIDs = append(retractedIDs, d.ID)
		} else {
			liveIDs = append(liveIDs, d.ID)
		}
	}
	out, err := a.store.ListLiveTargets(ctx, liveIDs)
	if err != nil {
		return nil, fmt.Errorf("listing targets: %w", err)
	}
	retracted, err := a.store.ListRetractedTargets(ctx, retractedIDs)
	if err != nil {
		return nil, fmt.Errorf("listing retracted targets: %w", err)
	}
	for id, rows := range retracted {
		out[id] = rows
	}
	return out, nil
}

// ListEligibleEngineers returns every person with whether enforcement can reach them.
func (a *Admin) ListEligibleEngineers(ctx context.Context, req *connect.Request[v1.ListEligibleEngineersRequest]) (*connect.Response[v1.ListEligibleEngineersResponse], error) {
	repo := normalizeRepository(req.Msg.GetRepository())
	if repo != "" {
		if err := validateRepository(repo); err != nil {
			return nil, err
		}
	}
	persons, err := a.dir.Persons(ctx)
	if err != nil {
		return nil, toConnect(fmt.Errorf("listing persons: %w", err))
	}
	ids := make([]string, 0, len(persons))
	for _, p := range persons {
		ids = append(ids, p.ID)
	}
	sums, err := a.store.ReportSummaries(ctx, ids)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &v1.ListEligibleEngineersResponse{}
	for _, p := range persons {
		e := &v1.EligibleEngineer{PersonId: p.ID, DisplayName: p.DisplayName}
		sum := sums[p.ID]
		switch {
		case sum == nil:
			e.IneligibleReason = status.ReasonNeverReported
		case repo != "" && !sum.KnownRepositories[repo]:
			e.IneligibleReason = status.ReasonRepoNotKnown
		default:
			e.Eligible = true
		}
		out.Engineers = append(out.Engineers, e)
	}
	return connect.NewResponse(out), nil
}

// GetArtifactFiles returns a definition's files at their repo-relative paths.
func (a *Admin) GetArtifactFiles(ctx context.Context, req *connect.Request[v1.GetArtifactFilesRequest]) (*connect.Response[v1.GetArtifactFilesResponse], error) {
	if err := validateID(req.Msg.GetArtifactId()); err != nil {
		return nil, err
	}
	def, err := a.store.GetArtifact(ctx, req.Msg.GetArtifactId(), false)
	if err != nil {
		return nil, toConnect(err)
	}
	targets, err := a.store.ListLiveTargets(ctx, []string{def.ID})
	if err != nil {
		return nil, toConnect(err)
	}
	seen := map[string]bool{}
	var repos []string
	for _, r := range targets[def.ID] {
		if r.Scope == v1.ArtifactScope_ARTIFACT_SCOPE_REPO_LOCAL && !seen[r.Repository] {
			seen[r.Repository] = true
			repos = append(repos, r.Repository)
		}
	}
	if len(repos) == 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errNoRepoLocal)
	}
	sort.Strings(repos)
	root, err := spec.InstallPath(def.IdeType, def.Type, def.Slug, v1.ArtifactScope_ARTIFACT_SCOPE_REPO_LOCAL)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	files, err := a.store.ListArtifactFiles(ctx, []string{def.ID})
	if err != nil {
		return nil, toConnect(err)
	}
	out := &v1.GetArtifactFilesResponse{Repositories: repos}
	for _, f := range files[def.ID] {
		content, err := a.blobs.Get(ctx, f.ContentHash)
		if err != nil {
			return nil, toConnect(fmt.Errorf("loading body %s: %w", f.ContentHash, err))
		}
		out.Files = append(out.Files, &v1.ArtifactFileContent{
			RelativePath: spec.FileInstallPath(root, f.RelativePath),
			Content:      string(content),
			IsPrimary:    f.IsPrimary,
		})
	}
	return connect.NewResponse(out), nil
}

// previewRoots render scope roots symbolically for GetInstallPreview.
var previewRoots = spec.Roots{Home: "~", RepoRoot: "<checkout>"}

// GetInstallPreview derives the install paths at one scope of an artifact that need not exist.
func (a *Admin) GetInstallPreview(_ context.Context, req *connect.Request[v1.GetInstallPreviewRequest]) (*connect.Response[v1.GetInstallPreviewResponse], error) {
	m := req.Msg
	if len(m.GetFileRelativePaths()) > MaxFiles {
		return nil, invalid(fmt.Errorf("at most %d files are allowed", MaxFiles))
	}
	scope := m.GetScope()
	if scope == v1.ArtifactScope_ARTIFACT_SCOPE_UNSPECIFIED {
		scope = v1.ArtifactScope_ARTIFACT_SCOPE_REPO_LOCAL
	}
	files, err := spec.Preview(m.GetIdeType(), m.GetType(), m.GetSlug(), scope, m.GetFileRelativePaths())
	if err != nil {
		return nil, invalid(err)
	}
	root, err := spec.InstallRoot(m.GetIdeType(), m.GetType(), scope, previewRoots)
	if err != nil {
		return nil, invalid(err)
	}
	out := &v1.GetInstallPreviewResponse{Root: root}
	for _, f := range files {
		out.Files = append(out.Files, &v1.InstallPreviewFile{RelativePath: f.RelativePath, IsPrimary: f.IsPrimary})
	}
	return connect.NewResponse(out), nil
}

// ListPersons returns the host directory's persons.
func (a *Admin) ListPersons(ctx context.Context, _ *connect.Request[v1.ListPersonsRequest]) (*connect.Response[v1.ListPersonsResponse], error) {
	ps, err := a.dir.Persons(ctx)
	if err != nil {
		return nil, toConnect(fmt.Errorf("listing persons: %w", err))
	}
	out := &v1.ListPersonsResponse{}
	for _, p := range ps {
		out.Persons = append(out.Persons, &v1.Person{PersonId: p.ID, DisplayName: p.DisplayName})
	}
	return connect.NewResponse(out), nil
}

// ListTeams returns the host directory's teams with their members.
func (a *Admin) ListTeams(ctx context.Context, _ *connect.Request[v1.ListTeamsRequest]) (*connect.Response[v1.ListTeamsResponse], error) {
	ts, err := a.dir.Teams(ctx)
	if err != nil {
		return nil, toConnect(fmt.Errorf("listing teams: %w", err))
	}
	out := &v1.ListTeamsResponse{}
	for _, t := range ts {
		members, err := a.dir.TeamMembers(ctx, t.ID)
		if err != nil {
			return nil, toConnect(fmt.Errorf("listing members of team %s: %w", t.ID, err))
		}
		out.Teams = append(out.Teams, &v1.Team{TeamId: t.ID, Name: t.Name, MemberPersonIds: members})
	}
	return connect.NewResponse(out), nil
}

// PauseEnforcement sets the kill switch.
func (a *Admin) PauseEnforcement(ctx context.Context, _ *connect.Request[v1.PauseEnforcementRequest]) (*connect.Response[v1.PauseEnforcementResponse], error) {
	if err := a.store.SetPaused(ctx, true); err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&v1.PauseEnforcementResponse{Paused: true}), nil
}

// ResumeEnforcement clears the kill switch.
func (a *Admin) ResumeEnforcement(ctx context.Context, _ *connect.Request[v1.ResumeEnforcementRequest]) (*connect.Response[v1.ResumeEnforcementResponse], error) {
	if err := a.store.SetPaused(ctx, false); err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&v1.ResumeEnforcementResponse{Paused: false}), nil
}

// GetSettings reports the kill switch, the hook opt-in and the window limits.
func (a *Admin) GetSettings(ctx context.Context, _ *connect.Request[v1.GetSettingsRequest]) (*connect.Response[v1.GetSettingsResponse], error) {
	paused, err := a.store.Paused(ctx)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&v1.GetSettingsResponse{
		EnforcementPaused:    paused,
		HooksEnabled:         a.cfg.AllowHooks,
		ServerVersion:        a.cfg.Version,
		EnforcementWindow:    durationpb.New(EnforcementWindow),
		MaxEnforcementWindow: durationpb.New(MaxEnforcementWindow),
	}), nil
}
