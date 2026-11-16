import createClient, { type Middleware } from "openapi-fetch";
import type { components, paths } from "./schema";
import { config } from "./config";
import { getToken, useAuth } from "./auth";

export type Schemas = components["schemas"];
export type Workflow = Schemas["Workflow"];
export type Execution = Schemas["Execution"];
export type StepRun = Schemas["StepRun"];
export type Alert = Schemas["Alert"];
export type Case = Schemas["Case"];
export type CaseTask = Schemas["CaseTask"];
export type Approval = Schemas["Approval"];
export type Source = Schemas["Source"];
export type Connection = Schemas["Connection"];
export type Principal = Schemas["Principal"];
export type FieldError = Schemas["FieldError"];
export type Dashboard = Schemas["Dashboard"];
export type DashboardRange = Schemas["DashboardRange"];
export type AlertPatch = Schemas["AlertPatch"];
export type TimelineEvent = Schemas["TimelineEvent"];
export type CaseAlert = Schemas["CaseAlert"];

// Problem is an RFC 9457 error from the API, thrown by unwrap.
export class Problem extends Error {
  constructor(public status: number, public title: string, public detail?: string, public errors: FieldError[] = []) {
    super(detail ? `${title}: ${detail}` : title);
  }
}

const auth: Middleware = {
  async onRequest({ request }) {
    const t = getToken();
    if (t) request.headers.set("Authorization", `Bearer ${t}`);
    return request;
  },
  async onResponse({ response }) {
    if (response.status === 401 && useAuth.getState().token) {
      useAuth.getState().clear();
      window.location.assign(`/login?expired=1&next=${encodeURIComponent(window.location.pathname)}`);
    }
    return response;
  },
};

let client: ReturnType<typeof createClient<paths>> | null = null;

export function api() {
  if (!client) {
    client = createClient<paths>({ baseUrl: config().apiBase });
    client.use(auth);
  }
  return client;
}

// unwrap turns an openapi-fetch result into data or a thrown Problem.
export async function unwrap<T>(p: Promise<{ data?: T; error?: unknown; response: Response }>): Promise<T> {
  const { data, error, response } = await p;
  if (error !== undefined || !response.ok) {
    const e = (error ?? {}) as { title?: string; detail?: string; errors?: FieldError[] };
    throw new Problem(response.status, e.title ?? response.statusText, e.detail, e.errors ?? []);
  }
  return data as T;
}

// raw is for endpoints openapi-fetch cannot express well (binary uploads).
export async function raw(path: string, init: RequestInit): Promise<Response> {
  const headers = new Headers(init.headers);
  const t = getToken();
  if (t) headers.set("Authorization", `Bearer ${t}`);
  const r = await fetch(config().apiBase + path, { ...init, headers });
  if (!r.ok) {
    const e = await r.json().catch(() => ({}));
    throw new Problem(r.status, e.title ?? r.statusText, e.detail, e.errors ?? []);
  }
  return r;
}
