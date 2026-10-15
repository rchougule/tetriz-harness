import { useRef, useState, type FormEvent } from "react";
import { useAdmin, useSession } from "../api/context";
import { errorMessage, isUnauthenticated } from "../api/errors";
import { Field } from "../components/Field";

export function SignIn() {
  const admin = useAdmin();
  const { signIn, signedOutReason, mock } = useSession();
  const [token, setToken] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const t = token.trim();
    if (!t) {
      setError("Enter the admin token.");
      return;
    }
    setPending(true);
    setError(null);
    try {
      // GetSettings is the cheapest admin read, so it proves the token.
      await admin.getSettings({}, { headers: { Authorization: `Bearer ${t}` } });
      signIn(t);
    } catch (err) {
      setError(isUnauthenticated(err) ? "That token was rejected." : `Could not reach the server: ${errorMessage(err)}`);
      // Focus returns to the field so its error, wired via aria-describedby, is read out.
      inputRef.current?.focus();
    } finally {
      setPending(false);
    }
  };

  return (
    <div className="signin">
      <form className="card" onSubmit={submit} noValidate>
        <div className="card-head" style={{ flexDirection: "column", alignItems: "flex-start", gap: 4 }}>
          <div className="brand" style={{ padding: 0 }}>
            <span className="brand-mark" aria-hidden="true">H</span>
            <h1 style={{ fontSize: 16 }}>Harness Control Plane</h1>
          </div>
          <p>Sign in with the admin token configured on this server.</p>
        </div>
        <div className="card-body">
          {signedOutReason === "rejected" && (
            <div className="callout callout-warn" role="alert">
              <span className="icon" aria-hidden="true">!</span>
              <span>Your session ended: the server rejected the admin token. Sign in again.</span>
            </div>
          )}
          <Field
            label="Admin token"
            error={error ?? undefined}
            hint={mock ? "Mock mode: any token except “invalid” is accepted." : "Kept in this browser tab only and cleared when the tab closes."}
          >
            {(p) => (
              <input
                {...p}
                ref={inputRef}
                className="input mono"
                type="password"
                autoComplete="current-password"
                value={token}
                onChange={(e) => setToken(e.target.value)}
                autoFocus
              />
            )}
          </Field>
          <button type="submit" className="btn btn-primary" disabled={pending} style={{ justifyContent: "center" }}>
            {pending ? "Checking…" : "Sign in"}
          </button>
        </div>
      </form>
    </div>
  );
}
