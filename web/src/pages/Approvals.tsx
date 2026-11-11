import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { Check, X } from "lucide-react";
import { api, unwrap, type Approval } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input, Select } from "@/components/ui/input";
import { Empty, ErrorBox, Loading, PageHeader, StatusBadge } from "@/components/common";
import { Badge } from "@/components/ui/badge";
import { ago } from "@/lib/utils";

export default function Approvals() {
  const [status, setStatus] = useState("pending");
  const q = useQuery({
    queryKey: ["approvals", status],
    queryFn: () => unwrap(api().GET("/approvals", { params: { query: { status: status || undefined, limit: 100, sort: status === "pending" ? "due_at" : "-created_at" } } })),
  });
  return (
    <div>
      <PageHeader title="Approvals" subtitle="Human decisions for approval nodes and for actions held back by policy (destructive, large sends)." />
      <Select className="mb-3 w-40" value={status} onChange={(e) => setStatus(e.target.value)}>
        {["pending", "approved", "rejected", "expired", "cancelled", ""].map((s) => <option key={s} value={s}>{s || "all"}</option>)}
      </Select>
      {q.isLoading ? <Loading /> : q.error ? <ErrorBox error={q.error} /> : q.data!.items.length === 0 ? <Card><Empty text="Nothing waits for a decision" /></Card> : (
        <div className="flex flex-col gap-2">{q.data!.items.map((a) => <ApprovalCard key={a.id} a={a} />)}</div>
      )}
    </div>
  );
}

function ApprovalCard({ a }: { a: Approval }) {
  const qc = useQueryClient();
  const [comment, setComment] = useState("");
  const decide = useMutation({
    mutationFn: (approved: boolean) => unwrap(api().POST("/approvals/{id}/decision", { params: { path: { id: a.id } }, body: { approved, comment } })),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["approvals"] }),
  });
  return (
    <Card>
      <CardContent className="flex flex-col gap-2 pt-4">
        <div className="flex flex-wrap items-center gap-2">
          <StatusBadge status={a.status} />
          {a.action && <Badge tone="orange">policy · {a.action}</Badge>}
          {a.risk && <Badge tone={a.risk === "high" ? "red" : "yellow"}>risk {a.risk}</Badge>}
          <span className="text-sm">{a.workflow_name} · node <code className="text-xs">{a.node_id}</code></span>
          <Link className="text-xs text-primary hover:underline" to={`/executions/${a.execution_id}`}>execution</Link>
          <span className="ml-auto text-xs text-muted-foreground">{a.status === "pending" ? `due ${ago(a.due_at)}` : `${a.decided_by ?? ""} ${ago(a.decided_at)}`}</span>
        </div>
        {a.message && <p className="text-sm">{a.message}</p>}
        <div className="text-xs text-muted-foreground">Approvers: {a.approvers.join(", ")}</div>
        {a.comment && <div className="text-xs">Comment: {a.comment}</div>}
        {a.status === "pending" && (
          <div className="flex gap-2">
            <Input placeholder="Comment (optional)" value={comment} onChange={(e) => setComment(e.target.value)} />
            <Button onClick={() => decide.mutate(true)} disabled={decide.isPending}><Check className="h-4 w-4" /> Approve</Button>
            <Button variant="destructive" onClick={() => decide.mutate(false)} disabled={decide.isPending}><X className="h-4 w-4" /> Reject</Button>
          </div>
        )}
        <ErrorBox error={decide.error} />
      </CardContent>
    </Card>
  );
}
