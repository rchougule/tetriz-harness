import { useMemo, useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router";
import { ArtifactScope, ArtifactType, IdeType } from "@harness-control-plane/gen/harnessctl/v1/types_pb";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import type { ArtifactSummary, GetArtifactResponse } from "@harness-control-plane/gen/harnessctl/v1/admin_pb";
import { useArtifact, useSetTargets, useSettings, useUpsertArtifact } from "../api/queries";
import { errorMessage, isNotFound } from "../api/errors";
import { AudiencePicker } from "../components/AudiencePicker";
import { EnforcementWindowNote } from "../components/EnforcementWindowNote";
import { Field } from "../components/Field";
import { InstallPreview } from "../components/InstallPreview";
import { ErrorState, Loading } from "../components/States";
import { useToast } from "../components/Toast";
import { draftToAudience } from "../lib/audience";
import { HOOK_EVENTS, IDE_OPTIONS, SCOPE_OPTIONS, TYPE_OPTIONS } from "../lib/labels";
import { enforceUntilDate, validateDraft, type ComposerDraft, type Errors, type WindowLimits } from "../lib/validate";

function blankDraft(): ComposerDraft {
  return {
    artifactId: "",
    ideType: IdeType.CLAUDE_CODE,
    type: ArtifactType.SKILL,
    slug: "",
    name: "",
    multiFile: false,
    inlineContent: "",
    files: [{ relativePath: "SKILL.md", content: "" }],
    hookEvent: "",
    hookMatcher: "",
    hookTimeout: "",
    scope: ArtifactScope.USER_GLOBAL,
    repository: "",
    audience: { mode: "org", personIds: [], teamIds: [] },
    enforceUntil: "",
  };
}

function draftFrom({ artifact: a, inlineContent, files }: GetArtifactResponse & { artifact: ArtifactSummary }): ComposerDraft {
  const multiFile = a.type === ArtifactType.SKILL && files.length > 1;
  return {
    ...blankDraft(),
    artifactId: a.artifactId,
    ideType: a.ideType,
    type: a.type,
    slug: a.slug,
    name: a.name,
    multiFile,
    inlineContent,
    files: multiFile ? files.map((f) => ({ relativePath: f.relativePath, content: f.content })) : [{ relativePath: "SKILL.md", content: inlineContent }],
    hookEvent: a.hookConfig?.event ?? "",
    hookMatcher: a.hookConfig?.matcher ?? "",
    hookTimeout: a.hookConfig?.timeoutSeconds ? String(a.hookConfig.timeoutSeconds) : "",
  };
}

export function ArtifactComposerPage() {
  const { id = "" } = useParams();
  const stored = useArtifact(id);
  if (!id) return <ArtifactComposer initial={blankDraft()} />;
  if (stored.isLoading) return <Loading />;
  if (stored.error && !isNotFound(stored.error)) return <ErrorState error={stored.error} onRetry={() => void stored.refetch()} />;
  const existing = stored.data?.artifact;
  if (!stored.data || !existing || existing.retractedAt)
    return (
      <div className="card card-body">
        <p>{existing?.retractedAt ? "This artifact was retracted and can no longer be edited." : "This artifact no longer exists."}</p>{" "}
        <Link to="/artifacts">Back to artifacts</Link>
      </div>
    );
  return <ArtifactComposer key={id} initial={draftFrom({ ...stored.data, artifact: existing })} existing={existing} />;
}

interface ComposerProps {
  initial: ComposerDraft;
  existing?: ArtifactSummary;
}

export function ArtifactComposer({ initial, existing }: ComposerProps) {
  const [d, setD] = useState<ComposerDraft>(initial);
  const [errors, setErrors] = useState<Errors>({});
  const [submitError, setSubmitError] = useState<string | null>(null);
  const upsert = useUpsertArtifact();
  const setTargets = useSetTargets();
  const navigate = useNavigate();
  const toast = useToast();
  const settings = useSettings();
  const isEdit = d.artifactId !== "";
  const isSkill = d.type === ArtifactType.SKILL;
  const isHook = d.type === ArtifactType.HOOK;
  const multi = isSkill && d.multiFile;
  const pending = upsert.isPending || setTargets.isPending;
  const s = settings.data;
  const maxDays = s?.maxEnforcementWindow ? Math.round(Number(s.maxEnforcementWindow.seconds) / 86_400) : undefined;
  const windowDays = s?.enforcementWindow ? Math.round(Number(s.enforcementWindow.seconds) / 86_400) : undefined;

  const set = <K extends keyof ComposerDraft>(k: K, v: ComposerDraft[K]) => setD((x) => ({ ...x, [k]: v }));
  const filePaths = useMemo(() => (multi ? d.files.map((f) => f.relativePath).filter(Boolean) : []), [multi, d.files]);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const draft = { ...d, multiFile: multi };
    const limits: WindowLimits | undefined = maxDays ? { now: Date.now(), maxDays } : undefined;
    const errs = validateDraft(draft, limits);
    setErrors(errs);
    setSubmitError(null);
    if (Object.keys(errs).length > 0) {
      requestAnimationFrame(() => document.querySelector<HTMLElement>('[aria-invalid="true"]')?.focus());
      return;
    }
    const until = limits && d.enforceUntil ? enforceUntilDate(d.enforceUntil, limits) : null;
    try {
      const res = await upsert.mutateAsync({
        artifactId: d.artifactId,
        ideType: d.ideType,
        type: d.type,
        slug: d.slug,
        name: d.name.trim(),
        inlineContent: multi ? "" : d.inlineContent,
        files: multi ? d.files : [],
        hookConfig: isHook
          ? { event: d.hookEvent, matcher: d.hookMatcher, timeoutSeconds: d.hookTimeout ? Number(d.hookTimeout) : 0 }
          : undefined,
        enforceUntil: until ? timestampFromDate(until) : undefined,
      });
      if (!isEdit) {
        try {
          await setTargets.mutateAsync({
            artifactId: res.artifactId,
            scope: d.scope,
            repository: d.scope === ArtifactScope.REPO_LOCAL ? d.repository : "",
            audience: draftToAudience(d.audience),
          });
        } catch (err) {
          toast(`Saved, but targeting failed: ${errorMessage(err)}`);
        }
      }
      toast(isEdit ? "Artifact saved" : "Artifact created");
      navigate(`/artifacts/${res.artifactId}`);
    } catch (err) {
      setSubmitError(errorMessage(err));
    }
  };

  const hookEvents = HOOK_EVENTS[d.ideType] ?? [];
  // A new artifact previews its first placement; an edit previews every scope it is placed at.
  const placed = [...new Set(existing?.placements.map((p) => p.scope) ?? [])].sort();
  const previewScopes = isEdit ? (placed.length ? placed : [ArtifactScope.USER_GLOBAL, ArtifactScope.REPO_LOCAL]) : [d.scope];

  return (
    <form onSubmit={submit} noValidate>
      <div className="crumb">
        <Link to="/artifacts">Artifacts</Link>
        {existing && (
          <>
            {" / "}
            <Link to={`/artifacts/${existing.artifactId}`}>{existing.name || existing.slug}</Link>
          </>
        )}
      </div>
      <div className="page-head">
        <div>
          <h1>{isEdit ? `Edit ${existing?.name || d.slug}` : "New artifact"}</h1>
          <p>
            {isEdit
              ? "Saving restarts the enforcement window. The tool and type of a saved artifact cannot change."
              : "Define the artifact and who receives it. You can change targets later on the artifact page."}
          </p>
        </div>
        <div className="row">
          <Link to={existing ? `/artifacts/${existing.artifactId}` : "/artifacts"} className="btn">
            Cancel
          </Link>
          <button type="submit" className="btn btn-primary" disabled={pending}>
            {pending ? "Saving…" : isEdit ? "Save" : "Create artifact"}
          </button>
        </div>
      </div>

      {submitError && (
        <div className="callout callout-danger" role="alert" style={{ marginBottom: 16 }}>
          <span className="icon" aria-hidden="true">✕</span>
          <span>{submitError}</span>
        </div>
      )}

      <div className="composer-grid">
        <div className="stack">
          <section className="card" aria-labelledby="sec-def">
            <div className="card-head">
              <h2 id="sec-def">Definition</h2>
            </div>
            <div className="card-body form-grid">
              <Field label="Tool" error={errors.ideType}>
                {(p) => (
                  <select {...p} className="select" value={d.ideType} disabled={isEdit} onChange={(e) => set("ideType", Number(e.target.value) as IdeType)}>
                    {IDE_OPTIONS.map((o) => (
                      <option key={o.value} value={o.value}>
                        {o.label}
                      </option>
                    ))}
                  </select>
                )}
              </Field>
              <Field label="Type" error={errors.type}>
                {(p) => (
                  <select {...p} className="select" value={d.type} disabled={isEdit} onChange={(e) => set("type", Number(e.target.value) as ArtifactType)}>
                    {TYPE_OPTIONS.map((o) => (
                      <option key={o.value} value={o.value}>
                        {o.label}
                      </option>
                    ))}
                  </select>
                )}
              </Field>
              <Field label="Slug" error={errors.slug} hint="Derives the install path. Lower-case letters, digits and dashes.">
                {(p) => (
                  <input {...p} className="input mono" value={d.slug} maxLength={100} spellCheck={false} autoComplete="off" onChange={(e) => set("slug", e.target.value)} />
                )}
              </Field>
              <Field label="Name" error={errors.name} hint="Shown to admins only; never used in a path.">
                {(p) => <input {...p} className="input" value={d.name} maxLength={200} onChange={(e) => set("name", e.target.value)} />}
              </Field>
            </div>
          </section>

          {isHook && (
            <section className="card" aria-labelledby="sec-hook">
              <div className="card-head">
                <h2 id="sec-hook">Hook activation</h2>
              </div>
              <div className="card-body stack">
                {settings.data?.hooksEnabled === false ? (
                  <div className="callout callout-danger" role="alert">
                    <span className="icon" aria-hidden="true">✕</span>
                    <span>
                      <strong>Hook artifacts are disabled on this server.</strong> Saving a hook is refused until an operator sets
                      HARNESS_ALLOW_HOOKS=true.
                    </span>
                  </div>
                ) : (
                  <div className="callout callout-warn" role="note">
                    <span className="icon" aria-hidden="true">!</span>
                    <span>
                      <strong>Hooks run code on every targeted machine.</strong>
                      {settings.data?.hooksEnabled ? " Hook enforcement is enabled on this server." : ""}
                    </span>
                  </div>
                )}
                <div className="form-grid">
                  <Field label="Event" error={errors.hookEvent} hint="In the tool's own hook vocabulary.">
                    {(p) => (
                      <select {...p} className="select" value={d.hookEvent} onChange={(e) => set("hookEvent", e.target.value)}>
                        <option value="">Choose an event</option>
                        {hookEvents.map((ev) => (
                          <option key={ev} value={ev}>
                            {ev}
                          </option>
                        ))}
                      </select>
                    )}
                  </Field>
                  <Field label="Matcher" hint="Narrows the subject, for example a tool name. Empty matches everything.">
                    {(p) => <input {...p} className="input mono" value={d.hookMatcher} onChange={(e) => set("hookMatcher", e.target.value)} />}
                  </Field>
                  <Field label="Timeout (seconds)" error={errors.hookTimeout} hint="Empty leaves the tool's default.">
                    {(p) => <input {...p} className="input" inputMode="numeric" value={d.hookTimeout} onChange={(e) => set("hookTimeout", e.target.value)} />}
                  </Field>
                </div>
                <p className="small muted">The command a hook runs is derived by the server from the tool, scope and slug; it always runs the placed script.</p>
              </div>
            </section>
          )}

          <section className="card" aria-labelledby="sec-body">
            <div className="card-head">
              <h2 id="sec-body">{isHook ? "Script" : "Content"}</h2>
              {isSkill && (
                <div className="row" role="radiogroup" aria-label="Skill files" style={{ marginLeft: "auto" }}>
                  <label className="choice" style={{ padding: "4px 10px" }}>
                    <input type="radio" name="multi" checked={!d.multiFile} onChange={() => set("multiFile", false)} /> Single SKILL.md
                  </label>
                  <label className="choice" style={{ padding: "4px 10px" }}>
                    <input type="radio" name="multi" checked={d.multiFile} onChange={() => set("multiFile", true)} /> Several files
                  </label>
                </div>
              )}
            </div>
            <div className="card-body stack">
              {!multi && (
                <Field label={isHook ? "Hook script" : isSkill ? "SKILL.md" : "Body"} error={errors.inlineContent}>
                  {(p) => (
                    <textarea
                      {...p}
                      className="textarea"
                      value={d.inlineContent}
                      spellCheck={false}
                      placeholder={isHook ? "#!/bin/sh\n" : ""}
                      onChange={(e) => set("inlineContent", e.target.value)}
                    />
                  )}
                </Field>
              )}
              {multi && (
                <>
                  {errors.files && (
                    <p className="small" style={{ color: "var(--danger-fg)" }} role="alert">
                      {errors.files}
                    </p>
                  )}
                  {d.files.map((f, i) => (
                    <div key={i} className="stack" style={{ gap: 8, borderTop: i ? "1px solid var(--border)" : 0, paddingTop: i ? 14 : 0 }}>
                      <div className="row" style={{ alignItems: "flex-end", flexWrap: "nowrap" }}>
                        <Field label={`File ${i + 1} path`} error={errors[`file.${i}`]} className="spacer">
                          {(p) => (
                            <input
                              {...p}
                              className="input mono"
                              value={f.relativePath}
                              spellCheck={false}
                              placeholder="scripts/run.sh"
                              onChange={(e) => set("files", d.files.map((x, j) => (j === i ? { ...x, relativePath: e.target.value } : x)))}
                            />
                          )}
                        </Field>
                        <button
                          type="button"
                          className="btn btn-ghost"
                          aria-label={`Remove file ${i + 1}`}
                          onClick={() => set("files", d.files.filter((_, j) => j !== i))}
                          style={{ marginBottom: errors[`file.${i}`] ? 24 : 0 }}
                        >
                          Remove
                        </button>
                      </div>
                      <Field label={`File ${i + 1} content`}>
                        {(p) => (
                          <textarea
                            {...p}
                            className="textarea"
                            style={{ minHeight: 110 }}
                            spellCheck={false}
                            value={f.content}
                            onChange={(e) => set("files", d.files.map((x, j) => (j === i ? { ...x, content: e.target.value } : x)))}
                          />
                        )}
                      </Field>
                    </div>
                  ))}
                  <div>
                    <button type="button" className="btn btn-sm" onClick={() => set("files", [...d.files, { relativePath: "", content: "" }])}>
                      Add file
                    </button>
                  </div>
                </>
              )}
            </div>
          </section>

          {!isEdit && (
            <section className="card" aria-labelledby="sec-target">
              <div className="card-head">
                <h2 id="sec-target">Target</h2>
                <p>One placement to start with; add more on the artifact page.</p>
              </div>
              <div className="card-body stack">
                <fieldset aria-describedby={errors.scope ? "scope-err" : undefined}>
                  <legend>Where</legend>
                  <div className="choice-row" role="radiogroup">
                    {SCOPE_OPTIONS.map((o) => (
                      <label key={o.value} className="choice">
                        <input type="radio" name="scope" checked={d.scope === o.value} onChange={() => set("scope", o.value)} />
                        <span>
                          <span style={{ fontWeight: 560 }}>{o.label}</span>
                          <br />
                          <span className="small muted">{o.hint}</span>
                        </span>
                      </label>
                    ))}
                  </div>
                  {errors.scope && (
                    <p id="scope-err" className="small" style={{ color: "var(--danger-fg)", marginTop: 6 }}>
                      {errors.scope}
                    </p>
                  )}
                </fieldset>
                {d.scope === ArtifactScope.REPO_LOCAL && (
                  <Field label="Repository" error={errors.repository} hint="owner/repo, for example example-org/example-repo.">
                    {(p) => (
                      <input {...p} className="input mono" value={d.repository} spellCheck={false} placeholder="example-org/example-repo" onChange={(e) => set("repository", e.target.value.trim())} />
                    )}
                  </Field>
                )}
                <AudiencePicker
                  value={d.audience}
                  onChange={(a) => set("audience", a)}
                  repository={d.scope === ArtifactScope.REPO_LOCAL ? d.repository : ""}
                  error={errors.audience}
                />
              </div>
            </section>
          )}
        </div>

        <aside className="stack">
          <section className="card" aria-labelledby="sec-preview">
            <div className="card-head">
              <h2 id="sec-preview">Install preview</h2>
            </div>
            <div className="card-body">
              <div className="stack">
                {previewScopes.map((scope) => (
                  <InstallPreview key={scope} ideType={d.ideType} type={d.type} slug={d.slug} filePaths={filePaths} scope={scope} />
                ))}
              </div>
            </div>
          </section>
          <section className="card" aria-labelledby="sec-window">
            <div className="card-head">
              <h2 id="sec-window">Enforcement window</h2>
            </div>
            <div className="card-body stack">
              <Field
                label="Enforce until"
                error={errors.enforceUntil}
                hint={
                  windowDays && maxDays
                    ? `Optional. Empty enforces for ${windowDays} days from this save; at most ${maxDays} days ahead.`
                    : "Optional. Empty uses the server's default window."
                }
              >
                {(p) => <input {...p} type="date" className="input" value={d.enforceUntil} onChange={(e) => set("enforceUntil", e.target.value)} />}
              </Field>
              {existing && <EnforcementWindowNote enforceUntil={existing.enforceUntil} />}
            </div>
          </section>
          {isEdit && (
            <section className="card card-body">
              <p className="small muted">
                Targets are edited on the <Link to={`/artifacts/${d.artifactId}`}>artifact page</Link>.
              </p>
            </section>
          )}
        </aside>
      </div>
    </form>
  );
}
