import { useInfiniteQuery } from "@tanstack/react-query";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { Bell, CalendarClock, Hand, Play, RotateCw, Webhook, Workflow as WorkflowIcon } from "lucide-react";
import { api, unwrap, type Execution } from "@/lib/api";
import { duration } from "@/lib/format";
import { useHotkeys } from "@/lib/hotkeys";
import { useFlash } from "@/lib/stream";
import { cn } from "@/lib/utils";
import { Page } from "@/components/Layout";
import { Empty, ErrorBox, PageHeader, SkeletonRows, StatusBadge, Time } from "@/components/common";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Segmented } from "@/components/ui/input";
import { useState } from "react";

const triggerIcon: Record<string, React.ElementType> = { alert: Bell, schedule: CalendarClock, cron: CalendarClock, webhook: Webhook, manual: Hand, subworkflow: WorkflowIcon, retry: RotateCw };

export default function Executions() {
  const [params, setParams] = useSearchParams();
  const nav = useNavigate();
  const status = params.get("status") ?? "";
  const workflow = params.get("workflow_id") ?? "";
  const list = useInfiniteQuery({
    queryKey: ["executions", "list", status, workflow],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) => unwrap(api().GET("/executions", { params: { query: { limit: 50, sort: "-created_at", ...(status ? { status } : {}), ...(workflow ? { workflow_id: workflow } : {}), ...(pageParam ? { cursor: pageParam } : {}) } } })),
    getNextPageParam: (l) => l.next_cursor || undefined,
    refetchInterval: 15_000,
  });
  const rows = list.data?.pages.flatMap((p) => p.items) ?? [];
  const [cursor, setCursor] = useState(0);
  useHotkeys({
    j: () => setCursor((c) => Math.min(rows.length - 1, c + 1)),
    k: () => setCursor((c) => Math.max(0, c - 1)),
    Enter: () => rows[cursor] && nav(`/executions/${rows[cursor].id}`),
  }, [rows, cursor]);
  return (
    <Page>
      <PageHeader title="Executions" subtitle="Workflow runs. Open one to follow it live, node by node.">
        <div className="flex flex-wrap items-center gap-2">
          <Segmented
            value={status}
            onChange={(v) => setParams((cur) => { const n = new URLSearchParams(cur); v ? n.set("status", v) : n.delete("status"); return n; }, { replace: true })}
            options={[
              { value: "", label: "All" },
              { value: "running,waiting,queued", label: "In flight" },
              { value: "succeeded", label: "Succeeded" },
              { value: "failed,timed_out", label: "Failed" },
              { value: "cancelled", label: "Cancelled" },
            ]}
          />
          {workflow && <Badge tone="accent" className="h-7 px-2">workflow {workflow}<button className="ml-1" onClick={() => setParams((c) => { const n = new URLSearchParams(c); n.delete("workflow_id"); return n; })} aria-label="Clear workflow filter">×</button></Badge>}
        </div>
      </PageHeader>
      <Card className="overflow-hidden">
        <div className="grid grid-cols-[120px_minmax(220px,1fr)_minmax(120px,180px)_minmax(120px,1fr)_100px_90px] items-center gap-3 border-b border-line px-4 py-2 text-2xs font-medium uppercase tracking-[0.05em] text-fg-3">
          <span>Status</span><span>Workflow</span><span>Trigger</span><span>Waiting on</span><span className="text-right">Started</span><span className="text-right">Duration</span>
        </div>
        {list.isLoading ? <SkeletonRows rows={10} /> : list.error ? <div className="p-4"><ErrorBox error={list.error} /></div> : !rows.length ? (
          <Empty icon={<Play />} title="No executions" text="Runs appear here when alerts trigger workflows, on schedules, or when you test-run a workflow." />
        ) : (
          <>
            {rows.map((e, i) => <ExecRow key={e.id} e={e} focused={i === cursor} />)}
            {list.hasNextPage && <div className="flex justify-center border-t border-line p-2"><Button size="sm" variant="ghost" loading={list.isFetchingNextPage} onClick={() => list.fetchNextPage()}>Load more</Button></div>}
          </>
        )}
      </Card>
    </Page>
  );
}

function ExecRow({ e, focused }: { e: Execution; focused: boolean }) {
  const flash = useFlash(e.id);
  const Icon = triggerIcon[e.trigger.type] ?? Play;
  const start = e.started_at ?? e.created_at;
  const secs = e.started_at ? ((e.ended_at ? new Date(e.ended_at) : new Date()).getTime() - new Date(e.started_at).getTime()) / 1000 : undefined;
  return (
    <Link
      to={`/executions/${e.id}`}
      className={cn("relative grid h-[var(--row-h)] grid-cols-[120px_minmax(220px,1fr)_minmax(120px,180px)_minmax(120px,1fr)_100px_90px] items-center gap-3 border-b border-line px-4 text-sm last:border-0 hover:bg-hover/60", focused && "shadow-[inset_2px_0_0_rgb(var(--accent))]")}
    >
      {flash && <span key={flash} className="pointer-events-none absolute inset-0 animate-flash" aria-hidden />}
      <StatusBadge status={e.status} />
      <span className="flex min-w-0 items-center gap-2">
        <span className="truncate font-medium">{e.workflow_name ?? e.workflow_id}</span>
        <span className="shrink-0 font-mono text-2xs text-fg-4">v{e.workflow_version}</span>
        {e.dry_run && <Badge tone="yellow" className="shrink-0">dry run</Badge>}
      </span>
      <span className="flex min-w-0 items-center gap-1.5 text-xs text-fg-2"><Icon className="size-3.5 shrink-0 text-fg-3" /><span className="truncate">{e.trigger.type}{e.trigger.user ? ` · ${e.trigger.user}` : ""}</span></span>
      <span className="truncate font-mono text-xs text-fg-3">{(e.pending_nodes ?? []).join(", ") || <span className="text-fg-4">—</span>}</span>
      <span className="text-right text-xs text-fg-3"><Time iso={start} /></span>
      <span className="tabular text-right text-xs text-fg-2">{secs !== undefined ? duration(secs) : "—"}</span>
    </Link>
  );
}
