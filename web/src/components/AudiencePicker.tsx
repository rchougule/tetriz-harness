import { useId } from "react";
import type { UseQueryResult } from "@tanstack/react-query";
import { useEligibleEngineers, useTeams } from "../api/queries";
import type { AudienceDraft, AudienceMode } from "../lib/validate";
import { ErrorCallout } from "./States";

interface Props {
  value: AudienceDraft;
  onChange: (v: AudienceDraft) => void;
  // Eligibility for a repository placement also requires a known checkout.
  repository: string;
  error?: string;
}

const MODES: { mode: AudienceMode; label: string; hint: string }[] = [
  { mode: "org", label: "Everyone", hint: "Every person, including people added later." },
  { mode: "teams", label: "Teams", hint: "Members resolve at read time, so later joiners inherit it." },
  { mode: "persons", label: "Specific people", hint: "Only the people you pick." },
];

function toggle(list: string[], id: string): string[] {
  return list.includes(id) ? list.filter((x) => x !== id) : [...list, id];
}

// Loading and failure for one directory list. Retry refetches the list only; the selection lives in the form.
function ListState({ query, what }: { query: UseQueryResult<unknown>; what: string }) {
  if (query.isFetching && !query.data)
    return (
      <p className="small muted" role="status">
        {`Loading ${what}…`}
      </p>
    );
  if (query.error) return <ErrorCallout error={query.error} what={`Could not load ${what}`} onRetry={() => void query.refetch()} />;
  return null;
}

export function AudiencePicker({ value, onChange, repository, error }: Props) {
  const name = useId();
  const errId = useId();
  const engineers = useEligibleEngineers(repository);
  const teams = useTeams();

  return (
    <fieldset aria-describedby={error ? errId : undefined}>
      <legend>Who</legend>
      <div className="choice-row" role="radiogroup">
        {MODES.map((m) => (
          <label key={m.mode} className="choice">
            <input
              type="radio"
              name={name}
              checked={value.mode === m.mode}
              onChange={() => onChange({ ...value, mode: m.mode })}
            />
            <span>
              <span style={{ fontWeight: 560 }}>{m.label}</span>
              <br />
              <span className="small muted">{m.hint}</span>
            </span>
          </label>
        ))}
      </div>

      {value.mode === "persons" && (
        <div style={{ marginTop: 10 }}>
          <ListState query={engineers} what="people" />
          {engineers.data && engineers.data.engineers.length === 0 && (
            <p className="small muted">No people yet. People and teams come from the host's directory.</p>
          )}
          <div className="check-list" role="group" aria-label="People">
            {engineers.data?.engineers.map((e) => (
              <label key={e.personId}>
                <input
                  type="checkbox"
                  checked={value.personIds.includes(e.personId)}
                  onChange={() => onChange({ ...value, personIds: toggle(value.personIds, e.personId) })}
                />
                <span>
                  {e.displayName}
                  {!e.eligible && (
                    <>
                      <br />
                      <span className="small muted">Not reachable yet: {e.ineligibleReason || "no agent report"}</span>
                    </>
                  )}
                </span>
              </label>
            ))}
          </div>
        </div>
      )}

      {value.mode === "teams" && (
        <div style={{ marginTop: 10 }}>
          <ListState query={teams} what="teams" />
          {teams.data && teams.data.teams.length === 0 && (
            <p className="small muted">No teams yet. Teams come from the host's directory.</p>
          )}
          <div className="check-list" role="group" aria-label="Teams">
            {teams.data?.teams.map((t) => (
              <label key={t.teamId}>
                <input
                  type="checkbox"
                  checked={value.teamIds.includes(t.teamId)}
                  onChange={() => onChange({ ...value, teamIds: toggle(value.teamIds, t.teamId) })}
                />
                <span>
                  {t.name}
                  <br />
                  <span className="small muted">
                    {t.memberPersonIds.length} member{t.memberPersonIds.length === 1 ? "" : "s"}
                  </span>
                </span>
              </label>
            ))}
          </div>
        </div>
      )}
      {error && (
        <p id={errId} className="small" style={{ color: "var(--danger-fg)", marginTop: 6 }}>
          {error}
        </p>
      )}
    </fieldset>
  );
}
