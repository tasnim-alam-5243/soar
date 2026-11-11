import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router-dom";
import { Plus } from "lucide-react";
import { api, unwrap } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input, Label } from "@/components/ui/input";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { Dialog, DialogContent } from "@/components/ui/dialog";
import { Empty, ErrorBox, Loading, PageHeader, Pager, StatusBadge } from "@/components/common";
import { ago } from "@/lib/utils";

const starter = {
  inputs: [{ name: "ip", from: 'alert.observables.exists(o, o.type == "ip") ? alert.observables.filter(o, o.type == "ip")[0].value : ""' }],
  nodes: [
    { id: "log", action: "notify.log", params: { message: '"triage of " + inputs.ip' } },
    { id: "open_case", action: "case.create", params: { title: '"Investigate " + inputs.ip', severity: 3 } },
  ],
  edges: ["log -> open_case"],
};

export default function Workflows() {
  const [cursor, setCursor] = useState<string | undefined>();
  const [creating, setCreating] = useState(false);
  const q = useQuery({
    queryKey: ["workflows", cursor],
    queryFn: () => unwrap(api().GET("/workflows", { params: { query: { limit: 50, sort: "-created_at", cursor } } })),
  });
  return (
    <div>
      <PageHeader title="Workflows" subtitle="Playbooks: published versions are immutable; executions pin their version."
        actions={<Button onClick={() => setCreating(true)}><Plus className="h-4 w-4" /> New workflow</Button>} />
      <Card>
        {q.isLoading ? <Loading /> : q.error ? <div className="p-3"><ErrorBox error={q.error} /></div> : q.data!.items.length === 0 ? <Empty text="No workflows yet" /> : (
          <>
            <Table>
              <THead><TR><TH>Name</TH><TH>Version</TH><TH>Status</TH><TH>Triggers</TH><TH>Created</TH></TR></THead>
              <TBody>
                {q.data!.items.map((w) => (
                  <TR key={w.id}>
                    <TD className="font-medium"><Link className="hover:underline" to={`/workflows/${w.id}`}>{w.name}</Link>
                      {w.description && <div className="text-xs font-normal text-muted-foreground">{w.description}</div>}</TD>
                    <TD>v{w.version}</TD>
                    <TD><StatusBadge status={w.status} /></TD>
                    <TD className="text-xs text-muted-foreground">{(w.triggers ?? []).map((t) => t.type + (t.cron ? ` (${t.cron})` : "")).join(", ") || "manual"}</TD>
                    <TD className="text-muted-foreground">{ago(w.created_at)} {w.created_by && `by ${w.created_by}`}</TD>
                  </TR>
                ))}
              </TBody>
            </Table>
            <Pager cursor={q.data!.next_cursor} onNext={setCursor} onFirst={() => setCursor(undefined)} hasPrev={!!cursor} />
          </>
        )}
      </Card>
      {creating && <NewWorkflow onClose={() => setCreating(false)} />}
    </div>
  );
}

function NewWorkflow({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient();
  const nav = useNavigate();
  const [name, setName] = useState("");
  const m = useMutation({
    mutationFn: () => unwrap(api().POST("/workflows", { body: { name, definition: starter } })),
    onSuccess: (w) => { qc.invalidateQueries({ queryKey: ["workflows"] }); nav(`/workflows/${w.id}`); },
  });
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent title="New workflow">
        <form className="flex flex-col gap-3" onSubmit={(e) => { e.preventDefault(); m.mutate(); }}>
          <div className="flex flex-col gap-1"><Label>Name</Label><Input value={name} onChange={(e) => setName(e.target.value)} placeholder="phishing_triage" required /></div>
          <ErrorBox error={m.error} />
          <Button type="submit" disabled={!name || m.isPending}>Create draft</Button>
        </form>
      </DialogContent>
    </Dialog>
  );
}
