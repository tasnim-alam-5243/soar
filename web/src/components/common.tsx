import * as React from "react";
import {
  AlertOctagon, AlertTriangle, Ban, CheckCircle2, CircleDashed, CircleDot, Clock, Copy, Check, Hourglass, Inbox, Loader2,
  PauseCircle, PlayCircle, RotateCw, ShieldAlert, ShieldCheck, ShieldQuestion, Timer, XCircle,
} from "lucide-react";
import { Problem } from "@/lib/api";
import { absolute, humanize, initials, pretty, relative } from "@/lib/format";
import { usePrefs } from "@/lib/prefs";
import { clampSev, sevColor, sevName } from "@/lib/severity";
import { cn, copy } from "@/lib/utils";
import { Tip } from "./ui/tooltip";

// ---------- page chrome ----------

export function PageHeader({ title, subtitle, actions, children, icon }: {
  title: React.ReactNode;
  subtitle?: React.ReactNode;
  actions?: React.ReactNode;
  children?: React.ReactNode;
  icon?: React.ReactNode;
}) {
  return (
    <div className="mb-5 flex flex-col gap-3">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex min-w-0 items-center gap-3">
          {icon && <span className="flex size-8 items-center justify-center rounded-lg border border-line bg-panel text-fg-2 [&_svg]:size-4">{icon}</span>}
          <div className="min-w-0">
            <h1 className="truncate text-lg font-semibold tracking-[-0.01em]">{title}</h1>
            {subtitle && <p className="truncate text-sm text-fg-3">{subtitle}</p>}
          </div>
        </div>
        {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
      </div>
      {children}
    </div>
  );
}

export function Loading({ label = "Loading", className }: { label?: string; className?: string }) {
  return (
    <div className={cn("flex items-center gap-2 p-6 text-sm text-fg-3", className)} role="status">
      <Loader2 className="size-4 animate-spin" /> {label}…
    </div>
  );
}

export function SkeletonRows({ rows = 8, className }: { rows?: number; className?: string }) {
  return (
    <div className={cn("flex flex-col", className)} aria-hidden>
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="flex h-[var(--row-h)] items-center gap-3 border-b border-line px-4 last:border-0">
          <div className="skeleton h-3 w-14" />
          <div className="skeleton h-3" style={{ width: `${30 + ((i * 37) % 40)}%` }} />
          <div className="skeleton ml-auto h-3 w-16" />
        </div>
      ))}
    </div>
  );
}

export function ErrorBox({ error, className }: { error: unknown; className?: string }) {
  if (!error) return null;
  const p = error instanceof Problem ? error : null;
  return (
    <div role="alert" className={cn("rounded-md border border-crit/30 bg-crit/[0.06] px-3 py-2.5 text-sm", className)}>
      <div className="flex items-center gap-2 font-medium text-crit">
        <AlertTriangle className="size-4 shrink-0" /> {p ? p.title : error instanceof Error ? error.message : String(error)}
        {p && <span className="tabular ml-auto text-2xs font-normal text-fg-4">HTTP {p.status}</span>}
      </div>
      {p?.detail && <p className="mt-1 text-fg-2">{p.detail}</p>}
      {p?.errors && p.errors.length > 0 && (
        <ul className="mt-1.5 flex flex-col gap-0.5 text-xs text-fg-2">
          {p.errors.map((e, i) => (
            <li key={i}>
              {e.path && <code className="mr-1 rounded bg-crit/10 px-1 text-crit">{e.path}</code>}
              {e.message}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

export function Empty({ title, text, icon, action, className }: { title?: string; text?: React.ReactNode; icon?: React.ReactNode; action?: React.ReactNode; className?: string }) {
  return (
    <div className={cn("flex flex-col items-center justify-center gap-2 px-6 py-14 text-center", className)}>
      <span className="mb-1 flex size-10 items-center justify-center rounded-xl border border-line bg-raised text-fg-3 [&_svg]:size-5">{icon ?? <Inbox />}</span>
      {title && <div className="text-sm font-medium">{title}</div>}
      {text && <div className="max-w-sm text-sm text-fg-3">{text}</div>}
      {action && <div className="mt-2">{action}</div>}
    </div>
  );
}

export function JsonView({ value, className }: { value: unknown; className?: string }) {
  const text = pretty(value);
  return (
    <div className={cn("group relative", className)}>
      <pre className="max-h-96 overflow-auto rounded-md border border-line bg-bg/60 p-3 font-mono text-[11.5px] leading-5 text-fg-2">{text}</pre>
      <CopyButton value={text} className="absolute right-1.5 top-1.5 opacity-0 transition-opacity group-hover:opacity-100" />
    </div>
  );
}

/** Field is one row of a property list: label left, value right. */
export function Field({ label, children, className }: { label: string; children: React.ReactNode; className?: string }) {
  return (
    <div className={cn("grid grid-cols-[112px_1fr] items-start gap-3 py-1.5 text-sm", className)}>
      <span className="pt-0.5 text-xs text-fg-3">{label}</span>
      <div className="min-w-0 text-fg">{children}</div>
    </div>
  );
}

export function Section({ title, children, actions, className }: { title: string; children: React.ReactNode; actions?: React.ReactNode; className?: string }) {
  return (
    <section className={cn("flex flex-col gap-2", className)}>
      <div className="flex items-center justify-between">
        <h4 className="label">{title}</h4>
        {actions}
      </div>
      {children}
    </section>
  );
}

export function CopyButton({ value, className, label = "Copy" }: { value: string; className?: string; label?: string }) {
  const [done, setDone] = React.useState(false);
  return (
    <Tip content={done ? "Copied" : label}>
      <button
        type="button"
        className={cn("rounded p-1 text-fg-3 hover:bg-hover hover:text-fg", className)}
        onClick={async (e) => {
          e.stopPropagation();
          if (await copy(value)) {
            setDone(true);
            setTimeout(() => setDone(false), 1200);
          }
        }}
        aria-label={label}
      >
        {done ? <Check className="size-3.5 text-good" /> : <Copy className="size-3.5" />}
      </button>
    </Tip>
  );
}

// ---------- time ----------

/** Time shows a relative time ("3m ago") with the exact time on hover (local or UTC per preference). */
export function Time({ iso, mode = "relative", className }: { iso?: string | null; mode?: "relative" | "absolute"; className?: string }) {
  const utc = usePrefs((s) => s.utc);
  const [, tick] = React.useReducer((x: number) => x + 1, 0);
  React.useEffect(() => {
    if (mode !== "relative" || !iso) return;
    const t = setInterval(tick, 30_000);
    return () => clearInterval(t);
  }, [mode, iso]);
  if (!iso) return <span className={cn("text-fg-4", className)}>—</span>;
  const exact = absolute(iso, utc);
  return (
    <Tip content={mode === "relative" ? exact : relative(iso)}>
      <time dateTime={iso} className={cn("tabular whitespace-nowrap", className)}>
        {mode === "relative" ? relative(iso) : exact}
      </time>
    </Tip>
  );
}

// ---------- severity ----------

/** SeverityGlyph is five ascending bars, filled up to the severity: readable without color. */
export function SeverityGlyph({ severity, className }: { severity?: number; className?: string }) {
  const s = clampSev(severity);
  return (
    <svg viewBox="0 0 15 12" className={cn("h-3 w-[15px] shrink-0", className)} aria-hidden>
      {[1, 2, 3, 4, 5].map((i) => (
        <rect key={i} x={(i - 1) * 3} y={12 - (i + 1) * 2} width="2" height={(i + 1) * 2} rx="0.6" fill={i <= s ? sevColor(s) : "rgb(var(--line-strong))"} />
      ))}
    </svg>
  );
}

export function SeverityBadge({ severity, compact }: { severity?: number; compact?: boolean }) {
  const s = clampSev(severity);
  return (
    <span className="inline-flex items-center gap-1.5 text-xs font-medium" style={{ color: sevColor(s) }} aria-label={`Severity ${s}, ${sevName[s]}`}>
      <SeverityGlyph severity={s} />
      {!compact && <span>{sevName[s]}</span>}
    </span>
  );
}

// ---------- status ----------

type Tone = "gray" | "accent" | "good" | "warn" | "serious" | "crit";
const statusMeta: Record<string, { tone: Tone; icon: React.ElementType; label?: string }> = {
  // executions and steps
  queued: { tone: "gray", icon: CircleDashed },
  running: { tone: "accent", icon: PlayCircle },
  waiting: { tone: "warn", icon: PauseCircle },
  retry_wait: { tone: "warn", icon: RotateCw, label: "Retry wait" },
  succeeded: { tone: "good", icon: CheckCircle2 },
  failed: { tone: "crit", icon: XCircle },
  cancelled: { tone: "gray", icon: Ban },
  timed_out: { tone: "serious", icon: Timer, label: "Timed out" },
  skipped: { tone: "gray", icon: CircleDashed },
  // approvals
  pending: { tone: "warn", icon: Hourglass },
  approved: { tone: "good", icon: ShieldCheck },
  rejected: { tone: "crit", icon: ShieldAlert },
  expired: { tone: "serious", icon: Clock },
  // alerts and cases
  new: { tone: "accent", icon: CircleDot },
  acknowledged: { tone: "warn", icon: CircleDot },
  open: { tone: "accent", icon: CircleDot },
  in_progress: { tone: "warn", icon: CircleDot, label: "In progress" },
  closed: { tone: "good", icon: CheckCircle2 },
  done: { tone: "good", icon: CheckCircle2 },
  // workflows
  draft: { tone: "gray", icon: CircleDashed },
  published: { tone: "good", icon: CheckCircle2 },
  archived: { tone: "gray", icon: Ban },
  // source health
  healthy: { tone: "good", icon: CheckCircle2 },
  ok: { tone: "good", icon: CheckCircle2 },
  degraded: { tone: "warn", icon: AlertTriangle },
  error: { tone: "crit", icon: AlertOctagon },
  down: { tone: "crit", icon: AlertOctagon },
  disabled: { tone: "gray", icon: Ban },
  unknown: { tone: "gray", icon: ShieldQuestion },
};

const toneText: Record<Tone, string> = {
  gray: "text-fg-3",
  accent: "text-accent",
  good: "text-good",
  warn: "text-warn",
  serious: "text-serious",
  crit: "text-crit",
};

export function StatusBadge({ status, className, iconOnly }: { status?: string | null; className?: string; iconOnly?: boolean }) {
  const s = status || "new";
  const m = statusMeta[s] ?? { tone: "gray" as Tone, icon: CircleDot };
  const Icon = m.icon;
  const label = m.label ?? humanize(s);
  const el = (
    <span className={cn("inline-flex items-center gap-1.5 whitespace-nowrap text-xs font-medium", className)}>
      <Icon className={cn("size-3.5 shrink-0", toneText[m.tone], s === "running" && "animate-live")} />
      {!iconOnly && <span className="text-fg-2">{label}</span>}
    </span>
  );
  return iconOnly ? <Tip content={label}>{el}</Tip> : el;
}

export const statusTone = (s?: string) => statusMeta[s ?? ""]?.tone ?? "gray";

// ---------- people ----------

const hues = [250, 200, 160, 30, 330, 280, 100, 10];
export function Avatar({ name, size = 20, className }: { name?: string | null; size?: number; className?: string }) {
  if (!name) {
    return <span className={cn("inline-flex shrink-0 items-center justify-center rounded-full border border-dashed border-line-strong text-fg-4", className)} style={{ width: size, height: size }} aria-label="Unassigned" />;
  }
  let h = 0;
  for (const ch of name) h = (h * 31 + ch.charCodeAt(0)) >>> 0;
  const hue = hues[h % hues.length];
  return (
    <Tip content={name}>
      <span
        className={cn("inline-flex shrink-0 select-none items-center justify-center rounded-full font-semibold text-white", className)}
        style={{ width: size, height: size, fontSize: size * 0.42, background: `oklch(0.58 0.12 ${hue})` }}
        aria-label={name}
      >
        {initials(name)}
      </span>
    </Tip>
  );
}

export function LiveDot({ status }: { status: "connecting" | "live" | "offline" }) {
  const label = status === "live" ? "Live: updates stream in" : status === "connecting" ? "Reconnecting to live updates" : "Live updates are off";
  return (
    <Tip content={label}>
      <span className="inline-flex items-center gap-1.5 text-2xs font-medium text-fg-3">
        <span className={cn("size-1.5 rounded-full", status === "live" ? "bg-good animate-live" : status === "connecting" ? "bg-warn" : "bg-fg-4")} />
        {status === "live" ? "Live" : status === "connecting" ? "Connecting" : "Offline"}
      </span>
    </Tip>
  );
}

/** ObservableChip shows type:value with copy. */
export function ObservableChip({ type, value, role }: { type: string; value: string; role?: string }) {
  return (
    <span className="group inline-flex max-w-full items-center gap-1 rounded-md border border-line bg-raised py-0.5 pl-1.5 pr-0.5 text-xs">
      <span className="text-2xs font-medium uppercase text-fg-4">{type}</span>
      <span className="truncate font-mono text-[11.5px] text-fg">{value}</span>
      {role && <span className="text-2xs text-fg-4">{role}</span>}
      <CopyButton value={value} className="p-0.5 opacity-50 group-hover:opacity-100" />
    </span>
  );
}
