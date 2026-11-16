import * as React from "react";
import { Link } from "react-router-dom";
import { Bar, BarChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis, Area, AreaChart } from "recharts";
import { ArrowDownRight, ArrowUpRight, Minus } from "lucide-react";
import { clock, dayClock, num } from "@/lib/format";
import { usePrefs } from "@/lib/prefs";
import { cn } from "@/lib/utils";
import { Tip } from "./ui/tooltip";

// Charts follow one system: thin marks with a 2px surface gap between stacked segments,
// recessive grid and axes, a legend for every multi-series chart, a hover tooltip on every
// plotted mark, and text in ink tokens (never in the series color).

export interface Series {
  key: string;
  label: string;
  color: string; // CSS color, normally rgb(var(--token))
}

// ---------- stat tile ----------

export function StatTile({ label, value, current, previous, goodWhen = "up", hint, to, spark, sparkColor, unit }: {
  label: string;
  value: React.ReactNode;
  current?: number | null;
  previous?: number | null;
  goodWhen?: "up" | "down" | "neither";
  hint?: string;
  to?: string;
  spark?: number[];
  sparkColor?: string;
  unit?: string;
}) {
  const delta = current !== undefined && current !== null && previous !== undefined && previous !== null ? current - previous : undefined;
  const pctChange = delta !== undefined && previous ? delta / previous : undefined;
  const dir = delta === undefined || delta === 0 ? 0 : delta > 0 ? 1 : -1;
  const good = goodWhen === "neither" || dir === 0 ? null : (dir > 0) === (goodWhen === "up");
  const body = (
    <div className={cn("group relative flex h-full min-w-0 flex-col gap-2 rounded-lg border border-line bg-panel p-4 transition-colors", to && "hover:border-line-strong hover:bg-raised")}>
      <div className="flex items-center justify-between gap-2">
        <span className="truncate text-xs font-medium text-fg-3">{label}</span>
        {hint && (
          <Tip content={hint}>
            <span className="cursor-help text-2xs text-fg-4">ⓘ</span>
          </Tip>
        )}
      </div>
      <div className="flex items-end justify-between gap-3">
        <div className="min-w-0 truncate text-[26px] font-semibold leading-8 tracking-[-0.02em]">
          {value}
          {unit && <span className="ml-0.5 text-base font-medium text-fg-3">{unit}</span>}
        </div>
        {spark && spark.length > 1 && <Sparkline data={spark} color={sparkColor ?? "rgb(var(--accent))"} width={72} height={26} />}
      </div>
      <div className="flex h-4 items-center gap-1 whitespace-nowrap text-xs">
        {delta === undefined ? (
          <span className="text-fg-4">no comparison</span>
        ) : (
          <>
            <span className={cn("inline-flex items-center gap-0.5 font-medium", good === null ? "text-fg-3" : good ? "text-good" : "text-crit")}>
              {dir > 0 ? <ArrowUpRight className="size-3.5" /> : dir < 0 ? <ArrowDownRight className="size-3.5" /> : <Minus className="size-3.5" />}
              {pctChange !== undefined && Number.isFinite(pctChange) ? `${Math.abs(Math.round(pctChange * 100))}%` : dir === 0 ? "0" : num(Math.abs(delta))}
            </span>
            <span className="truncate text-fg-4">vs previous period</span>
          </>
        )}
      </div>
    </div>
  );
  return to ? <Link to={to} className="block h-full focus-visible:rounded-lg">{body}</Link> : body;
}

export function Sparkline({ data, color, width = 84, height = 28 }: { data: number[]; color: string; width?: number; height?: number }) {
  const rows = data.map((v, i) => ({ i, v }));
  const id = React.useId().replace(/:/g, "");
  return (
    <div style={{ width, height }} className="shrink-0" aria-hidden>
      <ResponsiveContainer width="100%" height="100%">
        <AreaChart data={rows} margin={{ top: 2, right: 0, bottom: 0, left: 0 }}>
          <defs>
            <linearGradient id={`sp${id}`} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor={color} stopOpacity={0.28} />
              <stop offset="100%" stopColor={color} stopOpacity={0} />
            </linearGradient>
          </defs>
          <Area type="monotone" dataKey="v" stroke={color} strokeWidth={1.5} fill={`url(#sp${id})`} isAnimationActive={false} dot={false} />
        </AreaChart>
      </ResponsiveContainer>
    </div>
  );
}

// ---------- legend ----------

export function Legend({ series, totals, hidden, onToggle }: { series: Series[]; totals?: Record<string, number>; hidden?: Set<string>; onToggle?: (k: string) => void }) {
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
      {series.map((s) => {
        const off = hidden?.has(s.key);
        return (
          <button
            key={s.key}
            type="button"
            onClick={() => onToggle?.(s.key)}
            disabled={!onToggle}
            aria-pressed={!off}
            className={cn("inline-flex items-center gap-1.5 text-xs text-fg-2 transition-opacity", off && "opacity-40", onToggle && "hover:text-fg")}
          >
            <span className="size-2.5 rounded-[3px]" style={{ background: s.color }} />
            {s.label}
            {totals && <span className="tabular text-fg-4">{num(totals[s.key] ?? 0)}</span>}
          </button>
        );
      })}
    </div>
  );
}

// ---------- stacked bars over time ----------

interface Point {
  t: string;
  counts: Record<string, number>;
}

function TimeTooltip({ active, payload, label, series, intervalSeconds }: {
  active?: boolean;
  payload?: { dataKey: string; value: number }[];
  label?: string;
  series: Series[];
  intervalSeconds: number;
}) {
  const utc = usePrefs((s) => s.utc);
  if (!active || !payload?.length || !label) return null;
  const byKey = Object.fromEntries(payload.map((p) => [p.dataKey, p.value]));
  const total = payload.reduce((a, p) => a + (p.value || 0), 0);
  const end = new Date(new Date(label).getTime() + intervalSeconds * 1000).toISOString();
  return (
    <div className="min-w-[180px] rounded-lg border border-line-strong bg-raised p-2.5 text-xs shadow-pop">
      <div className="mb-1.5 flex items-center justify-between gap-3">
        <span className="font-medium text-fg">{intervalSeconds >= 86400 / 4 ? dayClock(label, utc) : clock(label, utc)} – {clock(end, utc)}</span>
        <span className="tabular text-fg-3">{num(total)}</span>
      </div>
      <div className="flex flex-col gap-1">
        {[...series].reverse().map((s) => (
          <div key={s.key} className="flex items-center gap-2">
            <span className="size-2 rounded-[2px]" style={{ background: s.color }} />
            <span className="flex-1 text-fg-2">{s.label}</span>
            <span className="tabular font-medium text-fg">{num(byKey[s.key] ?? 0)}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

export function StackedBars({ data, series, intervalSeconds, height = 220, hidden }: {
  data: Point[];
  series: Series[];
  intervalSeconds: number;
  height?: number | string; // a number of px, or "100%" to fill a flex container
  hidden?: Set<string>;
}) {
  const utc = usePrefs((s) => s.utc);
  const rows = React.useMemo(() => data.map((p) => ({ t: p.t, ...Object.fromEntries(series.map((s) => [s.key, p.counts?.[s.key] ?? 0])) })), [data, series]);
  const visible = series.filter((s) => !hidden?.has(s.key));
  const multiDay = data.length > 1 && new Date(data[data.length - 1].t).getTime() - new Date(data[0].t).getTime() > 86400_000;
  const tick = (v: string) => (multiDay ? dayClock(v, utc) : clock(v, utc));
  return (
    <div style={{ height }} className="w-full">
      <ResponsiveContainer width="100%" height="100%">
        <BarChart data={rows} margin={{ top: 6, right: 4, bottom: 0, left: -18 }} barCategoryGap="18%">
          <CartesianGrid vertical={false} stroke="rgb(var(--grid))" />
          <XAxis
            dataKey="t"
            tickFormatter={tick}
            tick={{ fill: "rgb(var(--fg-3))", fontSize: 11 }}
            axisLine={{ stroke: "rgb(var(--axis))" }}
            tickLine={false}
            minTickGap={28}
            interval="preserveStartEnd"
          />
          <YAxis allowDecimals={false} tick={{ fill: "rgb(var(--fg-3))", fontSize: 11 }} axisLine={false} tickLine={false} width={44} tickFormatter={(v) => num(v)} />
          <Tooltip cursor={{ fill: "rgb(var(--fg) / 0.05)" }} content={<TimeTooltip series={visible} intervalSeconds={intervalSeconds} />} isAnimationActive={false} />
          {visible.map((s, i) => (
            <Bar
              key={s.key}
              dataKey={s.key}
              stackId="a"
              fill={s.color}
              stroke="rgb(var(--panel))"
              strokeWidth={1}
              radius={i === visible.length - 1 ? [3, 3, 0, 0] : 0}
              isAnimationActive={false}
              maxBarSize={28}
            />
          ))}
        </BarChart>
      </ResponsiveContainer>
    </div>
  );
}

// ---------- ranked list ----------

export function TopList({ items, color = "rgb(var(--accent))", empty = "Nothing in this range", to, mono }: {
  items: { value: string; count: number }[];
  color?: string;
  empty?: string;
  to?: (value: string) => string;
  mono?: boolean;
}) {
  if (!items.length) return <div className="py-6 text-center text-sm text-fg-4">{empty}</div>;
  const max = Math.max(...items.map((i) => i.count), 1);
  return (
    <ol className="flex flex-col gap-1">
      {items.map((it, i) => {
        const inner = (
          <div className="relative flex h-8 items-center gap-3 overflow-hidden rounded-md px-2.5 transition-colors hover:bg-hover">
            <span
              className="absolute inset-y-1 left-0 rounded-r-[4px] opacity-[0.14]"
              style={{ width: `${Math.max(2, (it.count / max) * 100)}%`, background: color }}
              aria-hidden
            />
            <span className="tabular relative w-3 text-2xs text-fg-4">{i + 1}</span>
            <span className={cn("relative min-w-0 flex-1 truncate text-sm", mono && "font-mono text-xs")} title={it.value}>{it.value || "—"}</span>
            <span className="tabular relative text-sm font-medium">{num(it.count)}</span>
          </div>
        );
        return <li key={it.value + i}>{to ? <Link to={to(it.value)}>{inner}</Link> : inner}</li>;
      })}
    </ol>
  );
}

/** Meter is a thin fill bar; the track is a lighter step of the same hue. */
export function Meter({ value, tone = "accent", className }: { value: number; tone?: "accent" | "warn" | "crit" | "good"; className?: string }) {
  const v = Math.max(0, Math.min(1, value));
  const color = `rgb(var(--${tone}))`;
  return (
    <div className={cn("h-1.5 w-full overflow-hidden rounded-full", className)} style={{ background: `rgb(var(--${tone}) / 0.15)` }} role="meter" aria-valuenow={Math.round(v * 100)} aria-valuemin={0} aria-valuemax={100}>
      <div className="h-full rounded-full transition-[width]" style={{ width: `${v * 100}%`, background: color }} />
    </div>
  );
}
