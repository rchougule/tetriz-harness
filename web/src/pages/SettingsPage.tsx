import { useState } from "react";
import { useSetPaused, useSettings } from "../api/queries";
import { errorMessage } from "../api/errors";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { ErrorState, Loading } from "../components/States";
import { useToast } from "../components/Toast";

export function SettingsPage() {
  const settings = useSettings();
  const setPaused = useSetPaused();
  const toast = useToast();
  const [confirm, setConfirm] = useState<"pause" | "resume" | null>(null);
  const paused = settings.data?.enforcementPaused ?? false;
  const days = (d?: { seconds: bigint }) => (d ? Math.round(Number(d.seconds) / 86_400) : 0);

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Settings</h1>
          <p>Server-wide controls.</p>
        </div>
      </div>
      <section className="card" aria-labelledby="kill-h" style={{ maxWidth: 720 }}>
        <div className="card-head">
          <div className="stack" style={{ gap: 2 }}>
            <h2 id="kill-h">Enforcement</h2>
            <p>The kill switch. Pausing tells every agent to make no writes. It removes nothing: files already placed stay exactly as they are.</p>
          </div>
        </div>
        {settings.isLoading && <Loading />}
        {settings.error && <ErrorState error={settings.error} onRetry={() => void settings.refetch()} />}
        {settings.data && (
          <div className="card-body row" style={{ justifyContent: "space-between" }}>
            <div className="row">
              <span className="muted">Current state</span>
              {paused ? (
                <span className="badge tone-progress">
                  <span className="badge-glyph" aria-hidden="true">‖</span>Paused
                </span>
              ) : (
                <span className="badge tone-success">
                  <span className="badge-glyph" aria-hidden="true">▶</span>Running
                </span>
              )}
            </div>
            {paused ? (
              <button type="button" className="btn btn-primary" onClick={() => setConfirm("resume")}>
                Resume enforcement
              </button>
            ) : (
              <button type="button" className="btn btn-danger" onClick={() => setConfirm("pause")}>
                Pause enforcement
              </button>
            )}
          </div>
        )}
      </section>

      {settings.data && (
        <section className="card" aria-labelledby="server-h" style={{ maxWidth: 720, marginTop: 16 }}>
          <div className="card-head">
            <div className="stack" style={{ gap: 2 }}>
              <h2 id="server-h">Server</h2>
              <p>Set in the server's configuration; read-only here.</p>
            </div>
          </div>
          <div className="card-body">
            <dl className="kv">
              <dt>Hook artifacts</dt>
              <dd>
                {settings.data.hooksEnabled ? (
                  <span className="badge tone-caution">
                    <span className="badge-glyph" aria-hidden="true">!</span>Enabled
                  </span>
                ) : (
                  <span className="badge tone-muted">
                    <span className="badge-glyph" aria-hidden="true">–</span>Disabled
                  </span>
                )}
                <div className="small muted" style={{ marginTop: 4 }}>
                  {settings.data.hooksEnabled
                    ? "Hooks run code on every targeted machine. Turn off with HARNESS_ALLOW_HOOKS=false."
                    : "Saving or targeting a hook is refused. Turn on with HARNESS_ALLOW_HOOKS=true."}
                </div>
              </dd>
              <dt>Enforcement window</dt>
              <dd>
                {days(settings.data.enforcementWindow)} days from each save, or a chosen date up to {days(settings.data.maxEnforcementWindow)} days ahead
              </dd>
              <dt>Version</dt>
              <dd>
                <code>{settings.data.serverVersion || "unknown"}</code>
              </dd>
            </dl>
          </div>
        </section>
      )}

      <ConfirmDialog
        open={confirm !== null}
        title={confirm === "pause" ? "Pause enforcement?" : "Resume enforcement?"}
        description={
          confirm === "pause"
            ? "Agents stop writing on their next sync until you resume. Nothing is retracted or deleted."
            : "Agents resume converging every enforced artifact on their next sync."
        }
        confirmLabel={confirm === "pause" ? "Pause enforcement" : "Resume enforcement"}
        danger={confirm === "pause"}
        pending={setPaused.isPending}
        error={setPaused.error ? errorMessage(setPaused.error) : null}
        onCancel={() => {
          setPaused.reset();
          setConfirm(null);
        }}
        onConfirm={() =>
          setPaused.mutate(confirm === "pause", {
            onSuccess: (nowPaused) => {
              toast(nowPaused ? "Enforcement paused" : "Enforcement resumed");
              setConfirm(null);
            },
          })
        }
      />
    </>
  );
}
