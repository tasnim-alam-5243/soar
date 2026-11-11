import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { api, unwrap } from "@/lib/api";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { ErrorBox, Loading, PageHeader, StatusBadge } from "@/components/common";
import { ago } from "@/lib/utils";

function Stat({ label, value, to, hint }: { label: string; value: React.ReactNode; to?: string; hint?: string }) {
  const body = (
    <Card className="h-full transition-colors hover:bg-accent/40">
      <CardHeader>
        <CardTitle className="text-xs font-medium text-muted-foreground">{label}</CardTitle>
      </CardHeader>
      <CardContent>
        <div className="text-2xl font-semibold">{value}</div>
        {hint && <div className="text-xs text-muted-foreground">{hint}</div>}
      </CardContent>
    </Card>
  );
  return to ? <Link to={to}>{body}</Link> : body;
}

function Bars({ data, colors }: { data: Record<string, number>; colors: Record<string, string> }) {
  const max = Math.max(1, ...Object.values(data));
  return (
    <div className="flex flex-col gap-1.5">
      {Object.entries(data).map(([k, v]) => (
        <div key={k} className="flex items-center gap-2 text-xs">
          <span className="w-20 text-muted-foreground">{k}</span>
          <div className="h-3 flex-1 rounded bg-muted">
            <div className={"h-3 rounded " + (colors[k] ?? "bg-primary")} style={{ width: `${(v / max) * 100}%` }} />
          </div>
          <span className="w-10 text-right tabular-nums">{v}</span>
        </div>
      ))}
    </div>
  );
}

const sevColors: Record<string, string> = { "1": "bg-slate-400", "2": "bg-blue-500", "3": "bg-amber-500", "4": "bg-orange-500", "5": "bg-red-600" };
const execColors: Record<string, string> = { succeeded: "bg-green-500", failed: "bg-red-500", running: "bg-blue-500", waiting: "bg-amber-500", queued: "bg-slate-400", cancelled: "bg-slate-400", timed_out: "bg-orange-500" };

export default function Dashboard() {
  const q = useQuery({ queryKey: ["dashboard"], queryFn: () => unwrap(api().GET("/dashboard")), refetchInterval: 15_000 });
  if (q.isLoading) return <Loading />;
  if (q.error) return <ErrorBox error={q.error} />;
  const d = q.data!;
  const pct = (v?: number) => (v === undefined ? "—" : `${Math.round(v * 100)}%`);
  return (
    <div>
      <PageHeader title="Dashboard" subtitle={`Updated ${ago(d.at)}`} />
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-5">
        <Stat label="Alerts (24 h)" value={d.alerts_last_24h} to="/alerts" />
        <Stat label="Open cases" value={d.open_cases} to="/cases" />
        <Stat label="Pending approvals" value={d.pending_approvals} to="/approvals" />
        <Stat label="MTTR (7 d)" value={d.mttr_hours === undefined ? "—" : `${d.mttr_hours.toFixed(1)} h`} hint="mean time to close a case" />
        <Stat label="Automation rate" value={pct(d.automation_rate)} hint="alerts that started a workflow" />
      </div>
      <div className="mt-4 grid gap-3 lg:grid-cols-3">
        <Card>
          <CardHeader><CardTitle>Alerts by severity (24 h)</CardTitle></CardHeader>
          <CardContent><Bars data={d.alerts_by_severity} colors={sevColors} /></CardContent>
        </Card>
        <Card>
          <CardHeader><CardTitle>Executions (24 h)</CardTitle></CardHeader>
          <CardContent><Bars data={d.executions_last_24h} colors={execColors} /></CardContent>
        </Card>
        <Card>
          <CardHeader><CardTitle>Queue health</CardTitle></CardHeader>
          <CardContent className="flex flex-col gap-2 text-sm">
            {(d.queues ?? []).map((s) => {
              const q = s as { name: string; messages: number; pending: number; bytes: number; max_bytes: number };
              const fill = q.max_bytes > 0 ? q.bytes / q.max_bytes : 0;
              return (
                <div key={q.name} className="flex items-center justify-between">
                  <span className="font-mono text-xs">{q.name}</span>
                  <span className={fill > 0.7 ? "text-red-600" : "text-muted-foreground"}>
                    {q.messages} msgs · {q.pending} pending{q.max_bytes > 0 ? ` · ${Math.round(fill * 100)}% full` : ""}
                  </span>
                </div>
              );
            })}
            {!d.queues?.length && <span className="text-muted-foreground">No queue information</span>}
          </CardContent>
        </Card>
      </div>
      <Card className="mt-4">
        <CardHeader><CardTitle>Sources</CardTitle></CardHeader>
        <CardContent className="grid gap-2 md:grid-cols-2">
          {(d.sources ?? []).map((s) => {
            const src = s as { name: string; enabled: boolean; adapter: string; status?: string; last_event_at?: string };
            return (
              <div key={src.name} className="flex items-center justify-between rounded-md border p-2 text-sm">
                <span>
                  {src.name} <span className="text-xs text-muted-foreground">({src.adapter})</span>
                </span>
                <span className="flex items-center gap-2 text-xs text-muted-foreground">
                  {src.enabled ? <StatusBadge status={src.status ?? "unknown"} /> : <StatusBadge status="disabled" />}
                  last event {ago(src.last_event_at)}
                </span>
              </div>
            );
          })}
        </CardContent>
      </Card>
      {d.errors && <div className="mt-4"><ErrorBox error={new Error(Object.entries(d.errors).map(([k, v]) => `${k}: ${v}`).join("; "))} /></div>}
    </div>
  );
}
