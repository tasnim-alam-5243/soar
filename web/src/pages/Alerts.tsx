import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "react-router-dom";
import { Briefcase } from "lucide-react";
import { api, unwrap, type Alert } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input, Select } from "@/components/ui/input";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { Dialog, DialogContent } from "@/components/ui/dialog";
import { Empty, ErrorBox, Field, JsonView, Loading, PageHeader, Pager, SeverityBadge } from "@/components/common";
import { Badge } from "@/components/ui/badge";
import { ago, ts } from "@/lib/utils";

export default function Alerts() {
  const [severity, setSeverity] = useState("");
  const [source, setSource] = useState("");
  const [cursor, setCursor] = useState<string | undefined>();
  const [open, setOpen] = useState<Alert | null>(null);
  const query = { limit: 50, sort: "-received_at" as const, severity: severity || undefined, source_id: source || undefined, cursor };
  const q = useQuery({
    queryKey: ["alerts", query],
    queryFn: () => unwrap(api().GET("/alerts", { params: { query: query as never } })),
  });
  return (
    <div>
      <PageHeader title="Alerts" subtitle="Normalized detections from every source; repeats are grouped (count)." />
      <div className="mb-3 flex flex-wrap gap-2">
        <Select value={severity} onChange={(e) => { setSeverity(e.target.value); setCursor(undefined); }} className="w-40">
          <option value="">All severities</option>
          {[5, 4, 3, 2, 1].map((s) => <option key={s} value={s}>Severity {s}</option>)}
        </Select>
        <Input placeholder="Source (e.g. wazuh-prod)" value={source} onChange={(e) => { setSource(e.target.value); setCursor(undefined); }} className="w-56" />
      </div>
      <Card>
        {q.isLoading ? <Loading /> : q.error ? <div className="p-3"><ErrorBox error={q.error} /></div> : q.data!.items.length === 0 ? <Empty text="No alerts" /> : (
          <>
            <Table>
              <THead><TR><TH>Severity</TH><TH>Title</TH><TH>Source</TH><TH>Count</TH><TH>Last seen</TH><TH>Case</TH></TR></THead>
              <TBody>
                {q.data!.items.map((a) => (
                  <TR key={a.id} className="cursor-pointer" onClick={() => setOpen(a)}>
                    <TD><SeverityBadge severity={a.severity} /></TD>
                    <TD className="max-w-md truncate font-medium">{a.title}</TD>
                    <TD className="text-muted-foreground">{a.source_id}</TD>
                    <TD className="tabular-nums">{a.occurrence_count}</TD>
                    <TD className="text-muted-foreground">{ago(a.last_seen ?? a.received_at)}</TD>
                    <TD>{a.case_id ? <Badge tone="blue">linked</Badge> : "—"}</TD>
                  </TR>
                ))}
              </TBody>
            </Table>
            <Pager cursor={q.data!.next_cursor} onNext={setCursor} onFirst={() => setCursor(undefined)} hasPrev={!!cursor} />
          </>
        )}
      </Card>
      {open && <AlertDialog alert={open} onClose={() => setOpen(null)} />}
    </div>
  );
}

function AlertDialog({ alert, onClose }: { alert: Alert; onClose: () => void }) {
  const nav = useNavigate();
  const qc = useQueryClient();
  const toCase = useMutation({
    mutationFn: () => unwrap(api().POST("/alerts/{id}/case", { params: { path: { id: alert.id } }, body: {} })),
    onSuccess: (c) => { qc.invalidateQueries({ queryKey: ["alerts"] }); nav(`/cases/${c.id}`); },
  });
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent title={alert.title} className="max-w-3xl">
        <div className="grid grid-cols-3 gap-3">
          <Field label="Severity"><SeverityBadge severity={alert.severity} /> <span className="text-xs text-muted-foreground">(vendor: {alert.original_severity ?? "—"})</span></Field>
          <Field label="Source">{alert.source_id}</Field>
          <Field label="Occurrences">{alert.occurrence_count}</Field>
          <Field label="Occurred">{ts(alert.occurred_at)}</Field>
          <Field label="Received">{ts(alert.received_at)}</Field>
          <Field label="External id"><code className="text-xs">{alert.external_id ?? "—"}</code></Field>
        </div>
        {alert.description && <p className="whitespace-pre-wrap rounded-md bg-muted p-2 font-mono text-xs">{alert.description}</p>}
        <div>
          <div className="mb-1 text-xs font-medium text-muted-foreground">Observables</div>
          <div className="flex flex-wrap gap-1.5">
            {(alert.observables ?? []).map((o, i) => <Badge key={i} tone="outline">{o.type}: {o.value}{o.role ? ` (${o.role})` : ""}</Badge>)}
            {!alert.observables?.length && <span className="text-sm text-muted-foreground">none</span>}
          </div>
        </div>
        {alert.mitre && <Field label="MITRE ATT&CK">{[...(alert.mitre.technique_ids ?? [])].join(", ") || "—"} {alert.mitre.tactics?.length ? `· ${alert.mitre.tactics.join(", ")}` : ""}</Field>}
        <details>
          <summary className="cursor-pointer text-xs text-muted-foreground">Vendor fields (ext)</summary>
          <JsonView value={alert.ext ?? {}} />
        </details>
        <ErrorBox error={toCase.error} />
        <div className="flex justify-end gap-2">
          {alert.case_id ? (
            <Button onClick={() => nav(`/cases/${alert.case_id}`)}><Briefcase className="h-4 w-4" /> Open case</Button>
          ) : (
            <Button onClick={() => toCase.mutate()} disabled={toCase.isPending}><Briefcase className="h-4 w-4" /> Create case</Button>
          )}
        </div>
      </DialogContent>
    </Dialog>
  );
}
