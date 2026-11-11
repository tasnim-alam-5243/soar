import { useState } from "react";
import { Navigate, useSearchParams } from "react-router-dom";
import { KeyRound, LogIn, ShieldCheck } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input, Label } from "@/components/ui/input";
import { oidc, signInOIDC, useAuth } from "@/lib/auth";

export default function Login() {
  const [params] = useSearchParams();
  const next = params.get("next") ?? "/";
  const token = useAuth((s) => s.token);
  const setKey = useAuth((s) => s.setAPIKey);
  const [key, setKeyInput] = useState("");
  if (token) return <Navigate to={next} replace />;
  const hasOIDC = !!oidc();
  return (
    <div className="flex min-h-screen items-center justify-center bg-muted/40 p-4">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <div className="flex items-center gap-2 text-lg font-semibold">
            <ShieldCheck className="h-5 w-5 text-primary" /> SOAR
          </div>
          <CardTitle className="font-normal text-muted-foreground">Sign in to continue</CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          {params.get("expired") && <p className="text-sm text-amber-600">Your session ended. Please sign in again.</p>}
          {hasOIDC && (
            <Button onClick={() => signInOIDC(next)}>
              <LogIn className="h-4 w-4" /> Sign in with single sign-on
            </Button>
          )}
          <form
            className="flex flex-col gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              if (key.trim()) setKey(key.trim());
            }}
          >
            <Label htmlFor="key">{hasOIDC ? "Or use an API key" : "API key"}</Label>
            <Input id="key" type="password" placeholder="soar_…" value={key} onChange={(e) => setKeyInput(e.target.value)} autoComplete="off" />
            <Button variant="outline" type="submit" disabled={!key.trim().startsWith("soar_")}>
              <KeyRound className="h-4 w-4" /> Use API key
            </Button>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}
