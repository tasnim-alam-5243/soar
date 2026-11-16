import { useMemo, useState } from "react";
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { Activity, ArrowRight, Bell, Gauge, Radio, ShieldCheck, TableProperties } from "lucide-react";
import { api, unwrap, type Dashboard, type DashboardRange } from "@/lib/api";
import { duration, num, pct } from "@/lib/format";
import { usePerms } from "@/lib/perms";
import { SEVERITIES, sevColor, sevName } from "@/lib/severity";
import { Page } from "@/components/Layout";
import { Panel } from "@/components/ui/card";
import { Segmented } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Legend, Meter, StackedBars, StatTile, TopList, type Series } from "@/components/charts";
import { Empty, ErrorBox, PageHeader, SeverityBadge, StatusBadge, Time } from "@/components/common";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";

type RangeName = "1h" | "24h" | "7d";

const sevSeries: Series[] = [1, 2, 3, 4, 5].map((s) => ({ key: String(s), label: sevName[s], color: sevColor(s) }));
// execution outcomes carry meaning, so they wear the status colors (bottom of the stack first)
const execGroups: Record<string, string[]> = { succeeded: ["succeeded"], active: ["queued", "running", "waiting"], cancelled: ["cancelled"], failed: ["failed", "timed_out"] };
const execSeries: Series[] = [
  { key: "succeeded", label: "Succeeded", color: "rgb(var(--good))" },
  { key: "active", label: "In flight", color: "rgb(var(--accent))" },
  { key: "cancelled", label: "Cancelled", color: "rgb(var(--fg-3))" },
  { key: "failed", label: "Failed or timed out", color: "rgb(var(--crit))" },
];

export default function Dashboard() {
  const [range, setRange] = useState<RangeName>("24h");
  const { can } = usePerms();
  const q = useQuery({
    queryKey: ["dashboard", range],
    queryFn: () => unwrap(api().GET("/dashboard", { params: { query: { range } } })),
    refetchInterval: 30_000,
    placeholderData: keepPreviousData,
  });
  const d = q.data;
  const r = d?.range as DashboardRange | undefined;

  return (
    <Page>
      <PageHeader
        title="Overview"
        subtitle={d ? <>Updated <Time iso={d.at} /></> : "Security operations at a glance"}
        actions={
          <Segmented<RangeName>
            value={range}
            onChange={setRange}
            options={[{ value: "1h", label: "1 hour" }, { value: "24h", label: "24 hours" }, { value: "7d", label: "7 days" }]}
          />
        }
      />
      {q.error ? <ErrorBox error={q.error} /> : !d || !r ? <DashboardSkeleton /> : <Body d={d} r={r} range={range} canCases={can("cases:read")} canApprovals={can("approvals:read")} />}
    </Page>
  );
}

function Body({ d, r, range, canCases, canApprovals }: { d: Dashboard; r: DashboardRange; range: RangeName; canCases: boolean; canApprovals: boolean }) {
  const [hiddenSev, setHiddenSev] = useState<Set<string>>(new Set());
  const [table, setTable] = useState(false);
  const alertTotals = useMemo(() => totals(r.alert_series), [r.alert_series]);
  const execSeriesData = useMemo(
    () => r.execution_series.map((p) => ({ t: p.t, counts: Object.fromEntries(Object.entries(execGroups).map(([k, sts]) => [k, sts.reduce((a, s) => a + (p.counts?.[s] ?? 0), 0)])) })),
    [r.execution_series],
  );
  const execTotals = useMemo(() => totals(execSeriesData), [execSeriesData]);
  const alertSpark = r.alert_series.map((p) => Object.values(p.counts ?? {}).reduce((a, b) => a + b, 0));
  const failSpark = execSeriesData.map((p) => p.counts.failed ?? 0);
  const rangeLabel = range === "1h" ? "last hour" : range === "24h" ? "last 24 hours" : "last 7 days";
  const toggle = (k: string) => setHiddenSev((s) => { const n = new Set(s); n.has(k) ? n.delete(k) : n.add(k); return n; });
  const fromParam = `from=${encodeURIComponent(r.from.replace(/\.\d+Z$/, "Z"))}`;

  return (
    <div className="flex flex-col gap-4">
      <div className="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-6">
        <StatTile label={`Alerts, ${rangeLabel}`} value={num(r.alerts.current)} current={r.alerts.current} previous={r.alerts.previous} goodWhen="neither" to="/alerts" spark={alertSpark} sparkColor={sevColor(4)} />
        <StatTile label="Open cases" value={num(d.open_cases)} current={r.cases_opened.current} previous={r.cases_opened.previous} goodWhen="down" hint={`${num(r.cases_opened.current)} opened in the ${rangeLabel}; the change compares cases opened with the period before`} to={canCases ? "/cases" : undefined} />
        <StatTile label="Mean time to resolve" value={r.mttr_hours.current == null ? "—" : duration(r.mttr_hours.current * 3600)} current={r.mttr_hours.current} previous={r.mttr_hours.previous} goodWhen="down" hint="Average age of the cases closed in this range" />
        <StatTile label="Automation rate" value={pct(r.automation_rate.current)} current={r.automation_rate.current} previous={r.automation_rate.previous} goodWhen="up" hint="Share of alerts that started a workflow" />
        <StatTile label="Pending approvals" value={num(d.pending_approvals)} goodWhen="down" to={canApprovals ? "/approvals" : undefined} hint="Human decisions workflows wait for, right now" />
        <StatTile label="Failed executions" value={num(r.failed_executions.current)} current={r.failed_executions.current} previous={r.failed_executions.previous} goodWhen="down" to="/executions?status=failed" spark={failSpark} sparkColor="rgb(var(--crit))" />
      </div>

      <div className="grid gap-4 xl:grid-cols-3">
        <Panel
          className="xl:col-span-2"
          title="Alert volume"
          subtitle={`By severity, ${rangeLabel} · ${duration(r.interval_seconds)} buckets`}
          icon={<Bell />}
          bodyClassName="flex flex-col"
          actions={
            <Button size="xs" variant="ghost" onClick={() => setTable((t) => !t)} aria-pressed={table} tip={table ? "Show the chart" : "Show as a table"}>
              <TableProperties /> {table ? "Chart" : "Table"}
            </Button>
          }
        >
          <div className="mb-3 flex flex-wrap items-end justify-between gap-3">
            <div>
              <div className="text-[44px] font-semibold leading-[48px] tracking-[-0.03em]">{num(r.alerts.current)}</div>
              <div className="text-xs text-fg-3">alerts received · {num(r.alerts.previous)} in the period before</div>
            </div>
            <Legend series={[...sevSeries].reverse()} totals={alertTotals} hidden={hiddenSev} onToggle={toggle} />
          </div>
          {table ? <SeriesTable data={r.alert_series} series={[...sevSeries].reverse()} /> : (
            <div className="min-h-[232px] flex-1">
              <StackedBars data={r.alert_series} series={sevSeries} intervalSeconds={r.interval_seconds} hidden={hiddenSev} height="100%" />
            </div>
          )}
        </Panel>
        <Panel title="Alerts by severity" subtitle="Received in this range" icon={<Gauge />}>
          <div className="flex flex-col gap-3">
            {SEVERITIES.map((s) => {
              const n = alertTotals[String(s)] ?? 0;
              const max = Math.max(1, ...Object.values(alertTotals));
              return (
                <Link key={s} to={`/alerts?severity=${s}&${fromParam}`} className="group flex flex-col gap-1.5 rounded-md p-1 -m-1 hover:bg-hover">
                  <div className="flex items-center justify-between text-sm">
                    <SeverityBadge severity={s} />
                    <span className="tabular font-medium">{num(n)}</span>
                  </div>
                  <div className="h-1.5 overflow-hidden rounded-full bg-hover group-hover:bg-line">
                    <div className="h-full rounded-full" style={{ width: `${(n / max) * 100}%`, background: sevColor(s) }} />
                  </div>
                </Link>
              );
            })}
          </div>
          <div className="mt-5 border-t border-line pt-4">
            <div className="label mb-2">Top rules</div>
            <TopList items={r.top_rules} color={sevColor(4)} to={(v) => `/alerts?q=${encodeURIComponent(v)}&${fromParam}`} />
          </div>
        </Panel>
      </div>

      <div className="grid gap-4 xl:grid-cols-3">
        <Panel className="xl:col-span-2" title="Workflow executions" subtitle={`${num(r.executions.current)} runs · ${rangeLabel}`} icon={<Activity />} actions={<Legend series={execSeries} totals={execTotals} />}>
          <StackedBars data={execSeriesData} series={execSeries} intervalSeconds={r.interval_seconds} height={200} />
        </Panel>
        <QueueHealth queues={(d.queues ?? []) as unknown as QueueRow[]} />
      </div>

      <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        <Panel title="Top sources" icon={<Radio />}>
          <TopList items={r.top_sources} to={(v) => `/alerts?source_id=${encodeURIComponent(v)}&${fromParam}`} mono />
        </Panel>
        <Panel title="Top hosts" icon={<Activity />}>
          <TopList items={r.top_hosts} to={(v) => `/alerts?q=${encodeURIComponent(v)}&${fromParam}`} mono />
        </Panel>
        <SourceHealth sources={(d.sources ?? []) as unknown as SourceRow[]} />
      </div>

      <div className="grid gap-4 xl:grid-cols-3">
        <LatestCritical className="xl:col-span-2" />
        {canApprovals && <MyApprovals />}
      </div>

      {d.errors && Object.keys(d.errors).length > 0 && (
        <ErrorBox error={new Error(`Some figures could not be computed: ${Object.entries(d.errors).map(([k, v]) => `${k} (${v})`).join("; ")}`)} />
      )}
    </div>
  );
}

function totals(points: { counts?: Record<string, number> }[]) {
  const t: Record<string, number> = {};
  for (const p of points) for (const [k, v] of Object.entries(p.counts ?? {})) t[k] = (t[k] ?? 0) + v;
  return t;
}

function SeriesTable({ data, series }: { data: { t: string; counts: Record<string, number> }[]; series: Series[] }) {
  return (
    <div className="max-h-[232px] overflow-auto rounded-md border border-line">
      <Table>
        <THead>
          <TR>
            <TH>Bucket start</TH>
            {series.map((s) => <TH key={s.key} className="text-right">{s.label}</TH>)}
          </TR>
        </THead>
        <TBody>
          {data.map((p) => (
            <TR key={p.t}>
              <TD className="text-xs"><Time iso={p.t} mode="absolute" /></TD>
              {series.map((s) => <TD key={s.key} className="tabular text-right">{num(p.counts?.[s.key] ?? 0)}</TD>)}
            </TR>
          ))}
        </TBody>
      </Table>
    </div>
  );
}

interface QueueRow { name: string; messages: number; pending: number; bytes: number; max_bytes: number }

function QueueHealth({ queues }: { queues: QueueRow[] }) {
  return (
    <Panel title="Queue health" subtitle="JetStream streams" icon={<Gauge />} actions={<Link to="/admin?tab=queues" className="text-xs text-fg-3 hover:text-fg">Details</Link>}>
      {!queues.length ? (
        <div className="py-6 text-center text-sm text-fg-4">No queue information (the API runs without NATS)</div>
      ) : (
        <div className="flex flex-col gap-4">
          {queues.map((q) => {
            const fill = q.max_bytes > 0 ? q.bytes / q.max_bytes : 0;
            const tone = q.name.includes("DLQ") ? (q.messages > 0 ? "crit" : "good") : fill > 0.7 ? "crit" : fill > 0.4 ? "warn" : "accent";
            return (
              <div key={q.name} className="flex flex-col gap-1.5">
                <div className="flex items-center justify-between text-sm">
                  <span className="font-mono text-xs">{q.name}</span>
                  <span className="tabular text-xs text-fg-3">
                    <span className="font-medium text-fg">{num(q.messages)}</span> msgs · {num(q.pending)} pending
                  </span>
                </div>
                <Meter value={q.max_bytes > 0 ? fill : q.messages > 0 ? 0.04 : 0} tone={tone as "accent"} />
                {q.max_bytes > 0 && <span className="text-2xs text-fg-4">{Math.round(fill * 100)}% of the stream's byte limit</span>}
              </div>
            );
          })}
        </div>
      )}
    </Panel>
  );
}

interface SourceRow { name: string; enabled: boolean; adapter: string; status?: string; last_event_at?: string; rate_per_sec?: number; error_rate?: number }

function SourceHealth({ sources }: { sources: SourceRow[] }) {
  return (
    <Panel title="Source health" icon={<Radio />} bodyClassName="p-0" actions={<Link to="/sources" className="text-xs text-fg-3 hover:text-fg">All sources</Link>}>
      {!sources.length ? (
        <div className="py-6 text-center text-sm text-fg-4">No sources configured</div>
      ) : (
        <ul className="divide-y divide-line">
          {sources.slice(0, 8).map((s) => (
            <li key={s.name} className="flex items-center gap-3 px-4 py-2.5">
              <StatusBadge status={s.enabled ? s.status || "unknown" : "disabled"} iconOnly />
              <div className="min-w-0 flex-1">
                <div className="truncate text-sm font-medium">{s.name}</div>
                <div className="text-2xs text-fg-3">{s.adapter}{s.rate_per_sec ? ` · ${s.rate_per_sec.toFixed(1)}/s` : ""}{s.error_rate ? ` · ${pct(s.error_rate)} errors` : ""}</div>
              </div>
              <span className="text-xs text-fg-3"><Time iso={s.last_event_at} /></span>
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}

function LatestCritical({ className }: { className?: string }) {
  const q = useQuery({
    queryKey: ["alerts", "latest-critical"],
    queryFn: () => unwrap(api().GET("/alerts", { params: { query: { min_severity: 4, status: "new,acknowledged,in_progress", limit: 8 } } })),
    refetchInterval: 30_000,
  });
  return (
    <Panel className={className} title="Latest critical and high alerts" subtitle="Still open" icon={<Bell />} bodyClassName="p-0" actions={<Link to="/alerts?severity=5,4&status=new,acknowledged,in_progress" className="inline-flex items-center gap-1 text-xs text-fg-3 hover:text-fg">Triage <ArrowRight className="size-3" /></Link>}>
      {q.error ? <div className="p-4"><ErrorBox error={q.error} /></div> : !q.data ? <div className="p-4"><div className="skeleton h-24" /></div> : !q.data.items.length ? (
        <Empty icon={<ShieldCheck />} title="All clear" text="No open critical or high alerts." />
      ) : (
        <ul className="divide-y divide-line">
          {q.data.items.map((a) => (
            <li key={a.id}>
              <Link to={`/alerts?alert=${a.id}`} className="flex items-center gap-3 px-4 py-2.5 hover:bg-hover/60">
                <SeverityBadge severity={a.severity} compact />
                <span className="min-w-0 flex-1 truncate text-sm">{a.title}</span>
                <span className="hidden shrink-0 font-mono text-2xs text-fg-4 sm:inline">{a.source_id}</span>
                {a.occurrence_count > 1 && <span className="tabular rounded bg-hover px-1.5 text-2xs text-fg-3">×{a.occurrence_count}</span>}
                <span className="w-16 shrink-0 text-right text-xs text-fg-3"><Time iso={a.last_seen ?? a.received_at} /></span>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}

function MyApprovals() {
  const q = useQuery({
    queryKey: ["approvals", "dashboard"],
    queryFn: () => unwrap(api().GET("/approvals", { params: { query: { status: "pending", limit: 6, sort: "due_at" } } })),
    refetchInterval: 30_000,
  });
  return (
    <Panel title="Waiting for a decision" icon={<ShieldCheck />} bodyClassName="p-0" actions={<Link to="/approvals" className="text-xs text-fg-3 hover:text-fg">All approvals</Link>}>
      {!q.data ? <div className="p-4"><div className="skeleton h-24" /></div> : !q.data.items.length ? (
        <Empty icon={<ShieldCheck />} title="Nothing to decide" text="Workflows are not waiting on anyone." />
      ) : (
        <ul className="divide-y divide-line">
          {q.data.items.map((a) => (
            <li key={a.id}>
              <Link to="/approvals" className="flex items-center gap-3 px-4 py-2.5 hover:bg-hover/60">
                <StatusBadge status={a.status} iconOnly />
                <div className="min-w-0 flex-1">
                  <div className="truncate text-sm">{a.message || a.workflow_name || a.node_id}</div>
                  <div className="truncate text-2xs text-fg-3">{a.workflow_name} · {a.node_id}</div>
                </div>
                <span className="text-xs text-fg-3">due <Time iso={a.due_at} /></span>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}

function DashboardSkeleton() {
  return (
    <div className="flex flex-col gap-4" aria-busy>
      <div className="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-6">
        {Array.from({ length: 6 }, (_, i) => <div key={i} className="skeleton h-[108px] rounded-lg" />)}
      </div>
      <div className="grid gap-4 xl:grid-cols-3">
        <div className="skeleton h-[360px] rounded-lg xl:col-span-2" />
        <div className="skeleton h-[360px] rounded-lg" />
      </div>
    </div>
  );
}
