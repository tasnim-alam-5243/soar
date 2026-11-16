// Runtime configuration, served as /config.json (nginx writes it from environment variables),
// so one image works in every environment.
export interface AppConfig {
  apiBase: string;
  oidc?: { authority: string; clientId: string; scope?: string };
}

let current: AppConfig = { apiBase: "/api/v1" };

export async function loadConfig(): Promise<AppConfig> {
  try {
    const r = await fetch("/config.json", { cache: "no-store" });
    if (r.ok) current = { ...current, ...(await r.json()) };
  } catch {
    /* defaults */
  }
  return current;
}

export const config = () => current;
