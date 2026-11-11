import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { api, unwrap } from "@/lib/api";
import { Card } from "@/components/ui/card";
import { Select } from "@/components/ui/input";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { Empty, ErrorBox, Loading, PageHeader, Pager, StatusBadge } from "@/components/common";
import { ago, dur } from "@/lib/utils";

export default function Executions() {
  const [status, setStatus] = useState("");
  const [cursor, setCursor] = useState<string | undefined>();
  const query = { limit: 50, sort: "-created_at" as const, status: status || undefined, cursor };
  const q = useQuery({ queryKey: ["executions", query], queryFn: () => unwrap(api().GET("/executions", { params: { query } })), refetchInterval: 10_000 });
  return (
    <div>
      <PageHeader title="Executions" subtitle="Workflow runs; open one to follow it live." />
      <div className="mb-3">
        <Select value={status} onChange={(e) => { setStatus(e.target.value); setCursor(undefined); }} className="w-48">
          <option value="">All statuses</option>
          {["queued", "running", "waiting", "succeeded", "failed", "cancelled", "timed_out"].map((s) => <option key={s}>{s}</option>)}
        </Select>
      </div>
      <Card>
        {q.isLoading ? <Loading /> : q.error ? <div className="p-3"><ErrorBox error={q.error} /></div> : q.data!.items.length === 0 ? <Empty text="No executions" /> : (
          <>
            <Table>
              <THead><TR><TH>Workflow</TH><TH>Status</TH><TH>Trigger</TH><TH>Pending</TH><TH>Started</TH><TH>Duration</TH></TR></THead>
              <TBody>
                {q.data!.items.map((e) => (
                  <TR key={e.id}>
                    <TD className="font-medium"><Link className="hover:underline" to={`/executions/${e.id}`}>{e.workflow_name ?? e.workflow_id} v{e.workflow_version}</Link>
                      {e.dry_run && <span className="ml-2 text-xs text-amber-600">dry run</span>}</TD>
                    <TD><StatusBadge status={e.status} /></TD>
                    <TD className="text-xs text-muted-foreground">{e.trigger.type}{e.trigger.user ? ` · ${e.trigger.user}` : ""}</TD>
                    <TD className="font-mono text-xs">{(e.pending_nodes ?? []).join(", ") || "—"}</TD>
                    <TD className="text-muted-foreground">{ago(e.started_at ?? e.created_at)}</TD>
                    <TD className="tabular-nums">{e.started_at ? dur(((e.ended_at ? new Date(e.ended_at) : new Date()).getTime() - new Date(e.started_at).getTime()) / 1000) : "—"}</TD>
                  </TR>
                ))}
              </TBody>
            </Table>
            <Pager cursor={q.data!.next_cursor} onNext={setCursor} onFirst={() => setCursor(undefined)} hasPrev={!!cursor} />
          </>
        )}
      </Card>
    </div>
  );
}
