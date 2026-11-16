import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams } from "react-router-dom";
import { toast } from "sonner";
import { ArrowLeft, Ban, GanttChart, RotateCcw, Workflow as WorkflowIcon } from "lucide-react";
import { api, unwrap, type StepRun } from "@/lib/api";
import type { Definition } from "@/lib/dsl";
import { duration, ms } from "@/lib/format";
import { requires, usePerms } from "@/lib/perms";
import { confirmDialog } from "@/components/ui/dialog";
import { cn } from "@/lib/utils";
import { Canvas } from "@/workflow/Canvas";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Panel } from "@/components/ui/card";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { CopyButton, ErrorBox, Field, JsonView, Loading, SkeletonRows, StatusBadge, Time, statusTone } from "@/components/common";
import { Tip } from "@/components/ui/tooltip";

interface NodeState { status: string; attempt?: number; output?: unknown; error?: { code: string; message?: string }; started_at?: string; ended_at?: string }

const terminal = ["succeeded", "failed", "cancelled", "timed_out"];

export default function ExecutionDetail() {
  const { id = "" } = useParams();
  const qc = useQueryClient();
  const nav = useNavigate();
  const { can } = usePerms();
  const [sel, setSel] = useState<string>();
  const ex = useQuery({
    queryKey: ["execution", id],
    queryFn: () => unwrap(api().GET("/executions/{id}", { params: { path: { id } } })),
    refetchInterval: (q) => (q.state.data && terminal.includes(q.state.data.status) ? false : 2000),
  });
  const wf = useQuery({
    queryKey: ["workflow", ex.data?.workflow_id],
    enabled: !!ex.data,
    queryFn: () => unwrap(api().GET("/workflows/{id}", { params: { path: { id: ex.data!.workflow_id } } })),
    staleTime: Infinity,
  });
  const steps = useQuery({
    queryKey: ["steps", id, ex.data?.updated_at],
    enabled: !!ex.data,
    queryFn: () => unwrap(api().GET("/executions/{id}/steps", { params: { path: { id }, query: { limit: 200 } } })),
    placeholderData: (p) => p,
  });
  const cancel = useMutation({
    mutationFn: () => unwrap(api().POST("/executions/{id}/cancel", { params: { path: { id } }, body: { reason: "cancelled from the UI" } })),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["execution", id] }); toast.success("Cancellation requested"); },
  });
  const retry = useMutation({
    mutationFn: () => unwrap(api().POST("/executions/{id}/retry", { params: { path: { id } } })),
    onSuccess: (e) => nav(`/executions/${e.id}`),
  });
  const state = (ex.data?.context ?? {}) as { nodes?: Record<string, NodeState>; inputs?: unknown; vars?: unknown; output?: unknown; alert?: unknown };
  const statuses = useMemo(() => Object.fromEntries(Object.entries(state.nodes ?? {}).map(([k, v]) => [k, { status: v.status, attempt: v.attempt }])), [state.nodes]);
  if (ex.isLoading) return <Loading />;
  if (ex.error) return <div className="p-6"><ErrorBox error={ex.error} /></div>;
  const e = ex.data!;
  const node = sel ? state.nodes?.[sel] : undefined;
  const nodeSteps = (steps.data?.items ?? []).filter((s) => s.node_id === sel);
  const secs = e.started_at ? ((e.ended_at ? new Date(e.ended_at) : new Date()).getTime() - new Date(e.started_at).getTime()) / 1000 : undefined;
  const live = !terminal.includes(e.status);

  return (
    <div className="flex h-full flex-col">
      <div className="flex min-h-14 shrink-0 flex-wrap items-center gap-3 border-b border-line bg-panel px-4 py-2">
        <Link to="/executions" className="rounded-md p-1.5 text-fg-3 hover:bg-hover hover:text-fg" aria-label="Back to executions"><ArrowLeft className="size-4" /></Link>
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <h1 className="truncate text-[15px] font-semibold">{e.workflow_name ?? e.workflow_id}</h1>
            <Link to={`/workflows/${e.workflow_id}`} className="rounded bg-hover px-1.5 font-mono text-2xs text-fg-2 hover:text-fg">v{e.workflow_version}</Link>
            <StatusBadge status={e.status} />
            {e.dry_run && <Badge tone="yellow">dry run</Badge>}
            {live && <span className="inline-flex items-center gap-1 text-2xs text-fg-3"><span className="size-1.5 animate-live rounded-full bg-accent" /> live</span>}
          </div>
          <div className="flex items-center gap-1 text-xs text-fg-3">
            <span className="font-mono">{e.id}</span><CopyButton value={e.id} className="p-0.5" />
            · started <Time iso={e.started_at ?? e.created_at} />{secs !== undefined && <> · {duration(secs)}</>}
          </div>
        </div>
        <div className="ml-auto flex items-center gap-2">
          {live && (
            <Button
              variant="danger-ghost"
              loading={cancel.isPending}
              disabled={!can("executions:cancel")}
              tip={can("executions:cancel") ? undefined : requires("executions:cancel")}
              onClick={async () => (await confirmDialog({ title: "Cancel this execution?", body: "Running steps finish; nothing else starts.", confirm: "Cancel execution", danger: true })) && cancel.mutate()}
            >
              <Ban /> Cancel
            </Button>
          )}
          {["failed", "cancelled", "timed_out"].includes(e.status) && (
            <Button variant="primary" onClick={() => retry.mutate()} loading={retry.isPending} disabled={!can("executions:run")} tip={can("executions:run") ? "Start a new execution with the same inputs" : requires("executions:run")}><RotateCcw /> Retry</Button>
          )}
        </div>
      </div>
      {(cancel.error || retry.error) && <div className="border-b border-line p-3"><ErrorBox error={cancel.error ?? retry.error} /></div>}
      {e.error && <div className="border-b border-crit/30 bg-crit/[0.06] px-4 py-2 text-sm text-crit">{e.error}</div>}

      <div className="flex min-h-0 flex-1">
        <div className="flex min-w-0 flex-1 flex-col">
          <div className="relative min-h-[320px] flex-1 bg-bg">
            {wf.data ? <Canvas def={wf.data.definition as Definition} statuses={statuses} selected={sel} onSelect={setSel} /> : <Loading label="Loading workflow" />}
          </div>
          <div className="h-[260px] shrink-0 overflow-auto border-t border-line bg-panel">
            <StepTimeline steps={steps.data?.items ?? []} start={e.started_at ?? e.created_at} end={e.ended_at} selected={sel} onSelect={setSel} loading={steps.isLoading} />
          </div>
        </div>
        <aside className="flex w-[min(420px,36vw)] shrink-0 flex-col overflow-y-auto border-l border-line bg-panel">
          {sel ? (
            <div className="flex flex-col gap-4 p-4">
              <div className="flex items-center justify-between">
                <div className="flex items-center gap-2"><WorkflowIcon className="size-4 text-fg-3" /><span className="font-semibold">{sel}</span></div>
                <Button size="xs" variant="ghost" onClick={() => setSel(undefined)}>Execution</Button>
              </div>
              {node ? (
                <>
                  <div className="flex flex-col">
                    <Field label="Status"><StatusBadge status={node.status} /> {node.attempt && node.attempt > 1 ? <span className="text-xs text-fg-3">attempt {node.attempt}</span> : null}</Field>
                    <Field label="Started"><Time iso={node.started_at} mode="absolute" /></Field>
                    <Field label="Ended"><Time iso={node.ended_at} mode="absolute" /></Field>
                  </div>
                  {node.error && <div className="rounded-md border border-crit/30 bg-crit/[0.06] p-3 text-sm"><code className="text-xs text-crit">{node.error.code}</code><p className="mt-1 text-fg-2">{node.error.message}</p></div>}
                  <Tabs defaultValue="output">
                    <TabsList>
                      <TabsTrigger value="output">Output</TabsTrigger>
                      <TabsTrigger value="input">Input</TabsTrigger>
                      <TabsTrigger value="attempts" count={nodeSteps.length}>Attempts</TabsTrigger>
                    </TabsList>
                    <TabsContent value="output"><JsonView value={node.output ?? null} /></TabsContent>
                    <TabsContent value="input">{nodeSteps[nodeSteps.length - 1]?.input ? <JsonView value={(nodeSteps[nodeSteps.length - 1].input as { value?: unknown }).value ?? nodeSteps[nodeSteps.length - 1].input} /> : <p className="text-sm text-fg-3">No recorded input (secrets are always redacted).</p>}</TabsContent>
                    <TabsContent value="attempts">
                      <div className="flex flex-col divide-y divide-line rounded-md border border-line">
                        {nodeSteps.map((s) => (
                          <div key={s.id} className="flex items-center gap-2 px-3 py-2 text-xs">
                            <StatusBadge status={s.status} />
                            <span className="text-fg-3">#{s.attempt}{s.item !== undefined ? ` item ${s.item}` : ""}</span>
                            <span className="ml-auto tabular text-fg-2">{ms(s.duration_ms)}</span>
                          </div>
                        ))}
                        {!nodeSteps.length && <div className="px-3 py-4 text-center text-xs text-fg-3">No step runs</div>}
                      </div>
                    </TabsContent>
                  </Tabs>
                </>
              ) : <p className="text-sm text-fg-3">This node has not run.</p>}
            </div>
          ) : (
            <div className="flex flex-col gap-4 p-4">
              <div className="flex flex-col">
                <Field label="Trigger">
                  <span className="text-sm">{e.trigger.type}</span>
                  {e.trigger.alert_id && <> · <Link className="font-mono text-xs text-accent hover:underline" to={`/alerts?alert=${e.trigger.alert_id}`}>{e.trigger.alert_id}</Link></>}
                  {e.trigger.retry_of && <> · retry of <Link className="font-mono text-xs text-accent hover:underline" to={`/executions/${e.trigger.retry_of}`}>{e.trigger.retry_of}</Link></>}
                  {e.trigger.parent_execution_id && <> · from <Link className="font-mono text-xs text-accent hover:underline" to={`/executions/${e.trigger.parent_execution_id}`}>parent</Link></>}
                </Field>
                {e.trigger.user && <Field label="Started by">{e.trigger.user}</Field>}
                <Field label="Started"><Time iso={e.started_at} mode="absolute" /></Field>
                <Field label="Ended"><Time iso={e.ended_at} mode="absolute" /></Field>
                {e.deadline_at && <Field label="Deadline"><Time iso={e.deadline_at} mode="absolute" /></Field>}
                {(e.pending_nodes?.length ?? 0) > 0 && <Field label="Waiting on"><span className="font-mono text-xs">{e.pending_nodes!.join(", ")}</span></Field>}
              </div>
              <Panel title="Inputs" bodyClassName="p-3"><JsonView value={state.inputs ?? {}} /></Panel>
              {state.output !== undefined && <Panel title="Output" bodyClassName="p-3"><JsonView value={state.output} /></Panel>}
              <Panel title="Variables" bodyClassName="p-3"><JsonView value={state.vars ?? {}} /></Panel>
              <p className="text-xs text-fg-4">Select a node on the canvas or in the timeline to see its input, output and attempts.</p>
            </div>
          )}
        </aside>
      </div>
    </div>
  );
}

/** StepTimeline draws each step run as a bar on the execution's time axis (a small Gantt). */
function StepTimeline({ steps, start, end, selected, onSelect, loading }: { steps: StepRun[]; start: string; end?: string; selected?: string; onSelect: (id: string) => void; loading: boolean }) {
  const t0 = new Date(start).getTime();
  const t1 = Math.max(end ? new Date(end).getTime() : Date.now(), ...steps.map((s) => new Date(s.ended_at ?? Date.now()).getTime()), t0 + 1000);
  const span = t1 - t0;
  const sorted = [...steps].sort((a, b) => new Date(a.started_at ?? a.created_at).getTime() - new Date(b.started_at ?? b.created_at).getTime());
  return (
    <div className="min-w-[560px]">
      <div className="sticky top-0 z-[1] flex items-center gap-2 border-b border-line bg-panel px-4 py-2">
        <GanttChart className="size-4 text-fg-3" />
        <span className="text-sm font-semibold">Step runs</span>
        <span className="text-xs text-fg-3">{steps.length} · {duration(span / 1000)} total</span>
        <div className="ml-auto flex gap-4 text-2xs text-fg-4"><span>0s</span><span>{duration(span / 2000)}</span><span>{duration(span / 1000)}</span></div>
      </div>
      {loading ? <SkeletonRows rows={3} /> : !sorted.length ? <div className="px-4 py-6 text-center text-sm text-fg-3">No steps have run yet.</div> : (
        <div className="py-1">
          {sorted.map((s) => {
            const a = new Date(s.started_at ?? s.created_at).getTime();
            const b = s.ended_at ? new Date(s.ended_at).getTime() : Date.now();
            const left = ((a - t0) / span) * 100;
            const width = Math.max(0.6, ((b - a) / span) * 100);
            const tone = statusTone(s.status);
            return (
              <button
                key={s.id}
                onClick={() => onSelect(s.node_id)}
                className={cn("grid w-full grid-cols-[200px_1fr_72px] items-center gap-3 px-4 py-1 text-left text-xs hover:bg-hover/60", selected === s.node_id && "bg-accent/[0.07]")}
              >
                <span className="flex min-w-0 items-center gap-1.5">
                  <StatusBadge status={s.status} iconOnly />
                  <span className="truncate font-mono">{s.node_id}{s.item !== undefined ? `[${s.item}]` : ""}</span>
                  {s.attempt > 1 && <span className="text-fg-4">#{s.attempt}</span>}
                </span>
                <span className="relative h-4 rounded bg-hover/60">
                  <Tip content={`${s.action ?? ""} · ${ms(s.duration_ms ?? b - a)}${(s.error as { code?: string } | undefined)?.code ? ` · ${(s.error as { code: string }).code}` : ""}`}>
                    <span
                      className={cn("absolute inset-y-0.5 rounded-[3px]", s.status === "running" && "animate-live")}
                      style={{ left: `${left}%`, width: `${width}%`, background: `rgb(var(--${tone === "gray" ? "fg-4" : tone}))` }}
                    />
                  </Tip>
                </span>
                <span className="tabular text-right text-fg-2">{ms(s.duration_ms ?? (s.ended_at ? b - a : undefined))}</span>
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}

