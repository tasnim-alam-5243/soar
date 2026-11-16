import { useState } from "react";
import { Navigate, useSearchParams } from "react-router-dom";
import { Info, KeyRound, LogIn } from "lucide-react";
import { Button } from "@/components/ui/button";
import { FormField, Input } from "@/components/ui/input";
import { Logo } from "@/components/Logo";
import { oidc, signInOIDC, useAuth } from "@/lib/auth";

export default function Login() {
  const [params] = useSearchParams();
  const next = params.get("next") ?? "/";
  const token = useAuth((s) => s.token);
  const setKey = useAuth((s) => s.setAPIKey);
  const [key, setKeyInput] = useState("");
  const [busy, setBusy] = useState(false);
  if (token) return <Navigate to={next} replace />;
  const hasOIDC = !!oidc();
  // PKCE needs WebCrypto, which browsers only offer on HTTPS or localhost
  const secure = window.isSecureContext;
  const validKey = key.trim().startsWith("soar_") && key.trim().length > 12;

  return (
    <div className="relative flex min-h-full items-center justify-center overflow-hidden bg-bg p-4">
      <div
        aria-hidden
        className="pointer-events-none absolute inset-0 opacity-60"
        style={{ background: "radial-gradient(600px 300px at 50% 0%, rgb(var(--accent) / 0.16), transparent 70%)" }}
      />
      <div className="relative w-full max-w-[380px]">
        <div className="mb-8 flex flex-col items-center gap-3 text-center">
          <Logo size={40} />
          <div>
            <h1 className="text-xl font-semibold tracking-[-0.01em]">Sign in to SOAR</h1>
            <p className="text-sm text-fg-3">Security orchestration, automation and response</p>
          </div>
        </div>
        <div className="flex flex-col gap-4 rounded-xl border border-line bg-panel p-6 shadow-pop">
          {params.get("expired") && (
            <div className="rounded-md border border-warn/30 bg-warn/10 px-3 py-2 text-sm text-warn">Your session ended. Sign in again to continue.</div>
          )}
          {hasOIDC && (
            <>
              <Button variant="primary" size="lg" disabled={!secure || busy} loading={busy} onClick={() => { setBusy(true); void signInOIDC(next).catch(() => setBusy(false)); }}>
                <LogIn /> Continue with single sign-on
              </Button>
              {!secure && (
                <div className="flex gap-2 rounded-md border border-line bg-bg/60 px-3 py-2 text-xs text-fg-3">
                  <Info className="mt-0.5 size-3.5 shrink-0" />
                  <span>
                    Single sign-on needs HTTPS or <code>localhost</code>. Use an API key here, or open SOAR through an SSH tunnel
                    (<code>ssh -L 8080:127.0.0.1:8080 -L 8180:127.0.0.1:8180 …</code>) and browse to <code>http://localhost:8080</code>.
                  </span>
                </div>
              )}
              <div className="flex items-center gap-3 text-2xs uppercase tracking-[0.08em] text-fg-4">
                <span className="h-px flex-1 bg-line" /> or <span className="h-px flex-1 bg-line" />
              </div>
            </>
          )}
          <form
            className="flex flex-col gap-3"
            onSubmit={(e) => {
              e.preventDefault();
              if (validKey) setKey(key.trim());
            }}
          >
            <FormField label="API key" hint="Kept in this tab only and cleared when you sign out.">
              <Input
                type="password"
                placeholder="soar_…"
                value={key}
                onChange={(e) => setKeyInput(e.target.value)}
                autoComplete="off"
                spellCheck={false}
                icon={<KeyRound />}
                autoFocus={!hasOIDC || !secure}
              />
            </FormField>
            <Button type="submit" variant={hasOIDC && secure ? "secondary" : "primary"} size="lg" disabled={!validKey}>
              Sign in with API key
            </Button>
          </form>
        </div>
        {!secure && location.protocol === "http:" && (
          <p className="mt-4 text-center text-xs text-fg-4">This connection is not encrypted. Use it on a trusted network only.</p>
        )}
      </div>
    </div>
  );
}
