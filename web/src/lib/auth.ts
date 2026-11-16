import { UserManager, WebStorageStateStore, type User } from "oidc-client-ts";
import { create } from "zustand";
import { config } from "./config";

// Two ways to sign in: OIDC (Keycloak, authorization code + PKCE) or an API key (soar_...),
// useful for service accounts and development. Tokens live in sessionStorage only.
const KEY_STORAGE = "soar.apikey";

let manager: UserManager | null = null;

export function oidc(): UserManager | null {
  const c = config().oidc;
  if (!c?.authority || !c.clientId) return null;
  if (!manager) {
    manager = new UserManager({
      authority: c.authority,
      client_id: c.clientId,
      redirect_uri: `${window.location.origin}/callback`,
      post_logout_redirect_uri: `${window.location.origin}/`,
      response_type: "code",
      scope: c.scope ?? "openid profile email",
      automaticSilentRenew: true,
      userStore: new WebStorageStateStore({ store: window.sessionStorage }),
    });
  }
  return manager;
}

interface AuthState {
  token: string | null;
  kind: "oidc" | "apikey" | null;
  name: string | null;
  setOIDC: (u: User) => void;
  setAPIKey: (k: string) => void;
  clear: () => void;
}

export const useAuth = create<AuthState>((set) => ({
  token: null,
  kind: null,
  name: null,
  setOIDC: (u) => set({ token: u.access_token, kind: "oidc", name: (u.profile.preferred_username as string) ?? u.profile.sub }),
  setAPIKey: (k) => {
    sessionStorage.setItem(KEY_STORAGE, k);
    set({ token: k, kind: "apikey", name: "API key" });
  },
  clear: () => {
    sessionStorage.removeItem(KEY_STORAGE);
    set({ token: null, kind: null, name: null });
  },
}));

export const getToken = () => useAuth.getState().token;

// restore restores a session after a reload; returns true when signed in.
export async function restore(): Promise<boolean> {
  const key = sessionStorage.getItem(KEY_STORAGE);
  if (key) {
    useAuth.getState().setAPIKey(key);
    return true;
  }
  const m = oidc();
  if (!m) return false;
  const u = await m.getUser();
  if (u && !u.expired) {
    useAuth.getState().setOIDC(u);
    m.events.addUserLoaded((nu) => useAuth.getState().setOIDC(nu));
    return true;
  }
  return false;
}

export async function signInOIDC(returnTo: string) {
  await oidc()?.signinRedirect({ state: { returnTo } });
}

export async function completeOIDC(): Promise<string> {
  const m = oidc();
  if (!m) return "/";
  const u = await m.signinRedirectCallback();
  useAuth.getState().setOIDC(u);
  m.events.addUserLoaded((nu) => useAuth.getState().setOIDC(nu));
  return ((u.state as { returnTo?: string } | undefined)?.returnTo) ?? "/";
}

export async function signOut() {
  const kind = useAuth.getState().kind;
  useAuth.getState().clear();
  if (kind === "oidc") await oidc()?.signoutRedirect();
  else window.location.assign("/login");
}
