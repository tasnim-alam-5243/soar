import { usePrefs } from "./prefs";

// Formatting shared by every page. Times follow the viewer's choice of local time or UTC.

const pad = (n: number) => String(n).padStart(2, "0");

/** absolute renders a timestamp as "2026-11-16 14:03:22" (+ " UTC" in UTC mode). */
export function absolute(iso?: string | null, utc = usePrefs.getState().utc, withSeconds = true): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  const [y, mo, da, h, mi, s] = utc
    ? [d.getUTCFullYear(), d.getUTCMonth() + 1, d.getUTCDate(), d.getUTCHours(), d.getUTCMinutes(), d.getUTCSeconds()]
    : [d.getFullYear(), d.getMonth() + 1, d.getDate(), d.getHours(), d.getMinutes(), d.getSeconds()];
  return `${y}-${pad(mo)}-${pad(da)} ${pad(h)}:${pad(mi)}${withSeconds ? ":" + pad(s) : ""}${utc ? " UTC" : ""}`;
}

/** clock renders only the time of day, for chart axes ("14:00"). */
export function clock(iso: string | number, utc = usePrefs.getState().utc): string {
  const d = new Date(iso);
  return utc ? `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}` : `${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/** dayClock renders "Mon 14:00" for multi-day axes. */
export function dayClock(iso: string | number, utc = usePrefs.getState().utc): string {
  const d = new Date(iso);
  const day = d.toLocaleDateString(undefined, { weekday: "short", timeZone: utc ? "UTC" : undefined });
  return `${day} ${clock(iso, utc)}`;
}

/** relative renders "3m ago" / "in 2h". */
export function relative(iso?: string | null, now = Date.now()): string {
  if (!iso) return "—";
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return "—";
  const s = (now - t) / 1000;
  if (Math.abs(s) < 10) return "just now";
  return s < 0 ? `in ${duration(-s)}` : `${duration(s)} ago`;
}

/** duration renders seconds compactly: 42s, 7m, 3h, 2d. */
export function duration(s: number): string {
  if (s < 60) return `${Math.round(s)}s`;
  if (s < 3600) return `${Math.round(s / 60)}m`;
  if (s < 86400) return `${(s / 3600).toFixed(s < 36000 ? 1 : 0).replace(/\.0$/, "")}h`;
  return `${(s / 86400).toFixed(s < 864000 ? 1 : 0).replace(/\.0$/, "")}d`;
}

/** ms renders a step duration: 840 ms, 2.4 s, 1m 12s. */
export function ms(v?: number | null): string {
  if (v === undefined || v === null) return "—";
  if (v < 1000) return `${Math.round(v)} ms`;
  if (v < 60_000) return `${(v / 1000).toFixed(1)} s`;
  return `${Math.floor(v / 60_000)}m ${Math.round((v % 60_000) / 1000)}s`;
}

const compactFmt = new Intl.NumberFormat(undefined, { notation: "compact", maximumFractionDigits: 1 });
const intFmt = new Intl.NumberFormat();

/** num renders 1,284 or 12.9K (compact past 10,000). */
export function num(v?: number | null): string {
  if (v === undefined || v === null || Number.isNaN(v)) return "—";
  return Math.abs(v) >= 10_000 ? compactFmt.format(v) : intFmt.format(v);
}

export const pct = (v?: number | null) => (v === undefined || v === null ? "—" : `${Math.round(v * 100)}%`);

export function bytes(v?: number | null): string {
  if (!v) return "0 B";
  const u = ["B", "KB", "MB", "GB", "TB"];
  const i = Math.min(u.length - 1, Math.floor(Math.log(v) / Math.log(1024)));
  return `${(v / 1024 ** i).toFixed(i ? 1 : 0)} ${u[i]}`;
}

/** humanize turns "in_progress" into "In progress". */
export const humanize = (s?: string | null) => (s ? s.charAt(0).toUpperCase() + s.slice(1).replace(/_/g, " ") : "—");

export const pretty = (v: unknown) => JSON.stringify(v, null, 2);

/** initials for avatars: "Dana Scully" -> "DS", "analyst" -> "AN". */
export function initials(name?: string | null): string {
  if (!name) return "?";
  const parts = name.replace(/[@._-]+/g, " ").trim().split(/\s+/);
  return (parts.length > 1 ? parts[0][0] + parts[1][0] : name.slice(0, 2)).toUpperCase();
}
