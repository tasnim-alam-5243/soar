import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router-dom";
import { Plus } from "lucide-react";
import { api, unwrap } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input, Label, Select, Textarea } from "@/components/ui/input";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { Dialog, DialogContent } from "@/components/ui/dialog";
import { Empty, ErrorBox, Loading, PageHeader, Pager, SeverityBadge, StatusBadge } from "@/components/common";
import { ago } from "@/lib/utils";

export default function Cases() {
  const [status, setStatus] = useState("open,in_progress");
  const [cursor, setCursor] = useState<string | undefined>();
  const [creating, setCreating] = useState(false);
  const query = { limit: 50, sort: "-created_at" as const, status: status || undefined, cursor };
  const q = useQuery({ queryKey: ["cases", query], queryFn: () => unwrap(api().GET("/cases", { params: { query: query as never } })) });
  return (
    <div>
      <PageHeader title="Cases" actions={<Button onClick={() => setCreating(true)}><Plus className="h-4 w-4" /> New case</Button>} />
      <div className="mb-3">
        <Select value={status} onChange={(e) => { setStatus(e.target.value); setCursor(undefined); }} className="w-48">
          <option value="open,in_progress">Open and in progress</option>
          <option value="open">Open</option>
          <option value="in_progress">In progress</option>
          <option value="closed">Closed</option>
          <option value="">All</option>
        </Select>
      </div>
      <Card>
        {q.isLoading ? <Loading /> : q.error ? <div className="p-3"><ErrorBox error={q.error} /></div> : q.data!.items.length === 0 ? <Empty text="No cases" /> : (
          <>
            <Table>
              <THead><TR><TH>Severity</TH><TH>Title</TH><TH>Status</TH><TH>Assignee</TH><TH>Alerts</TH><TH>Created</TH></TR></THead>
              <TBody>
                {q.data!.items.map((c) => (
                  <TR key={c.id}>
                    <TD><SeverityBadge severity={c.severity} /></TD>
                    <TD className="font-medium"><Link className="hover:underline" to={`/cases/${c.id}`}>{c.title}</Link></TD>
                    <TD><StatusBadge status={c.status} /></TD>
                    <TD>{c.assignee || "—"}</TD>
                    <TD className="tabular-nums">{c.alert_ids?.length ?? 0}</TD>
                    <TD className="text-muted-foreground">{ago(c.created_at)}</TD>
                  </TR>
                ))}
              </TBody>
            </Table>
            <Pager cursor={q.data!.next_cursor} onNext={setCursor} onFirst={() => setCursor(undefined)} hasPrev={!!cursor} />
          </>
        )}
      </Card>
      {creating && <NewCase onClose={() => setCreating(false)} />}
    </div>
  );
}

function NewCase({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient();
  const nav = useNavigate();
  const [title, setTitle] = useState("");
  const [summary, setSummary] = useState("");
  const [severity, setSeverity] = useState(3);
  const m = useMutation({
    mutationFn: () => unwrap(api().POST("/cases", { body: { title, summary, severity } })),
    onSuccess: (c) => { qc.invalidateQueries({ queryKey: ["cases"] }); nav(`/cases/${c.id}`); },
  });
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent title="New case">
        <form className="flex flex-col gap-3" onSubmit={(e) => { e.preventDefault(); m.mutate(); }}>
          <div className="flex flex-col gap-1"><Label>Title</Label><Input value={title} onChange={(e) => setTitle(e.target.value)} required /></div>
          <div className="flex flex-col gap-1"><Label>Severity</Label>
            <Select value={severity} onChange={(e) => setSeverity(Number(e.target.value))}>{[1, 2, 3, 4, 5].map((s) => <option key={s} value={s}>{s}</option>)}</Select>
          </div>
          <div className="flex flex-col gap-1"><Label>Summary</Label><Textarea className="font-sans" value={summary} onChange={(e) => setSummary(e.target.value)} /></div>
          <ErrorBox error={m.error} />
          <Button type="submit" disabled={!title || m.isPending}>Create</Button>
        </form>
      </DialogContent>
    </Dialog>
  );
}
