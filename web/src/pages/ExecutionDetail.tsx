import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams } from "react-router-dom";
import { Ban, RotateCcw } from "lucide-react";
import { api, unwrap } from "@/lib/api";
import type { Definition } from "@/lib/dsl";
import { Canvas } from "@/workflow/Canvas";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { ErrorBox, Field, JsonView, Loading, PageHeader, StatusBadge } from "@/components/common";
import { ts } from "@/lib/utils";

interface NodeState { status: string; attempt?: number; output?: unknown; error?: { code: string; message?: string }; started_at?: string; ended_at?: string }

const terminal = ["succeeded", "failed", "cancelled", "timed_out"];

export default function ExecutionDetail() {
  const { id = "" } = useParams();
  const qc = useQueryClient();
  const nav = useNavigate();
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
  });
  const steps = useQuery({
    queryKey: ["steps", id, ex.data?.updated_at],
    enabled: !!ex.data,
    queryFn: () => unwrap(api().GET("/executions/{id}/steps", { params: { path: { id }, query: { limit: 200 } } })),
  });
  const cancel = useMutation({
    mutationFn: () => unwrap(api().POST("/executions/{id}/cancel", { params: { path: { id } }, body: { reason: "cancelled from the UI" } })),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["execution", id] }),
  });
  const retry = useMutation({
    mutationFn: () => unwrap(api().POST("/executions/{id}/retry", { params: { path: { id } } })),
    onSuccess: (e) => nav(`/executions/${e.id}`),
  });
  const state = (ex.data?.context ?? {}) as { nodes?: Record<string, NodeState>; inputs?: unknown; vars?: unknown; output?: unknown; alert?: unknown };
  const statuses = useMemo(() => Object.fromEntries(Object.entries(state.nodes ?? {}).map(([k, v]) => [k, { status: v.status, attempt: v.attempt }])), [state.nodes]);
  if (ex.isLoading) return <Loading />;
  if (ex.error) return <ErrorBox error={ex.error} />;
  const e = ex.data!;
  const node = sel ? state.nodes?.[sel] : undefined;
  return (
    <div>
      <PageHeader
        title={`${e.workflow_name ?? e.workflow_id} v${e.workflow_version}`}
        subtitle={`${e.id}${e.dry_run ? " · dry run" : ""}`}
        actions={
          <>
            <StatusBadge status={e.status} />
            {!terminal.includes(e.status) && <Button variant="outline" onClick={() => cancel.mutate()} disabled={cancel.isPending}><Ban className="h-4 w-4" /> Cancel</Button>}
            {["failed", "cancelled", "timed_out"].includes(e.status) && <Button variant="outline" onClick={() => retry.mutate()} disabled={retry.isPending}><RotateCcw className="h-4 w-4" /> Retry</Button>}
          </>
        }
      />
      <ErrorBox error={cancel.error ?? retry.error} />
      {e.error && <div className="mb-3 rounded-md bg-red-50 p-2 text-sm text-red-700 dark:bg-red-950 dark:text-red-200">{e.error}</div>}
      <div className="grid gap-3 lg:grid-cols-[1fr_380px]">
        <Card className="h-[480px] overflow-hidden">
          {wf.data ? <Canvas def={wf.data.definition as Definition} statuses={statuses} selected={sel} onSelect={setSel} /> : <Loading label="Loading workflow" />}
        </Card>
        <Card className="max-h-[480px] overflow-auto">
          <CardHeader><CardTitle>{sel ? `Node ${sel}` : "Execution"}</CardTitle></CardHeader>
          <CardContent className="flex flex-col gap-3">
            {sel && node ? (
              <>
                <Field label="Status"><StatusBadge status={node.status} /> {node.attempt && node.attempt > 1 ? `attempt ${node.attempt}` : ""}</Field>
                {node.error && <Field label="Error"><code className="text-xs text-red-600">{node.error.code}</code> {node.error.message}</Field>}
                <Field label="Started">{ts(node.started_at)}</Field>
                <Field label="Ended">{ts(node.ended_at)}</Field>
                <Field label="Output"><JsonView value={node.output ?? null} /></Field>
              </>
            ) : sel ? <span className="text-sm text-muted-foreground">This node has not run.</span> : (
              <>
                <Field label="Trigger">{e.trigger.type}{e.trigger.alert_id && <> · alert <code className="text-xs">{e.trigger.alert_id}</code></>}{e.trigger.retry_of && <> · retry of <Link className="text-primary" to={`/executions/${e.trigger.retry_of}`}>{e.trigger.retry_of}</Link></>}</Field>
                <Field label="Started">{ts(e.started_at)}</Field>
                <Field label="Ended">{ts(e.ended_at)}</Field>
                <Field label="Inputs"><JsonView value={state.inputs ?? {}} /></Field>
                {state.output !== undefined && <Field label="Output"><JsonView value={state.output} /></Field>}
                <Field label="Variables"><JsonView value={state.vars ?? {}} /></Field>
              </>
            )}
          </CardContent>
        </Card>
      </div>
      <Card className="mt-3">
        <CardHeader><CardTitle>Step runs</CardTitle></CardHeader>
        <Table>
          <THead><TR><TH>Node</TH><TH>Action</TH><TH>Attempt</TH><TH>Status</TH><TH>Duration</TH><TH>Ended</TH><TH>Error</TH></TR></THead>
          <TBody>
            {(steps.data?.items ?? []).map((s) => (
              <TR key={s.id} className="cursor-pointer" onClick={() => setSel(s.node_id)}>
                <TD className="font-mono text-xs">{s.node_id}{s.item ? `[${s.item}]` : ""}</TD>
                <TD className="font-mono text-xs">{s.action}</TD>
                <TD>{s.attempt}</TD>
                <TD><StatusBadge status={s.status} /></TD>
                <TD className="tabular-nums">{s.duration_ms ?? 0} ms</TD>
                <TD className="text-muted-foreground">{ts(s.ended_at)}</TD>
                <TD className="text-xs text-red-600">{(s.error as { code?: string } | undefined)?.code ?? ""}</TD>
              </TR>
            ))}
          </TBody>
        </Table>
      </Card>
    </div>
  );
}
