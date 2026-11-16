import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { toast } from "sonner";
import { AlarmClock, Check, ExternalLink, ShieldAlert, ShieldCheck, Users, X } from "lucide-react";
import { api, unwrap, type Approval } from "@/lib/api";
import { requires, usePerms } from "@/lib/perms";
import { useFlash } from "@/lib/stream";
import { cn } from "@/lib/utils";
import { Page } from "@/components/Layout";
import { Empty, ErrorBox, JsonView, PageHeader, SkeletonRows, StatusBadge, Time } from "@/components/common";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Input, Segmented } from "@/components/ui/input";

export default function Approvals() {
  const [status, setStatus] = useState("pending");
  const q = useQuery({
    queryKey: ["approvals", "list", status],
    queryFn: () => unwrap(api().GET("/approvals", { params: { query: { ...(status ? { status } : {}), limit: 100, sort: status === "pending" ? "due_at" : "-created_at" } } })),
    refetchInterval: status === "pending" ? 20_000 : false,
  });
  return (
    <Page className="max-w-[1100px]">
      <PageHeader title="Approvals" subtitle="Human decisions for approval nodes and for actions policy holds back (destructive actions, large sends).">
        <Segmented
          className="self-start"
          value={status}
          onChange={setStatus}
          options={[{ value: "pending", label: "Pending" }, { value: "approved", label: "Approved" }, { value: "rejected", label: "Rejected" }, { value: "expired", label: "Expired" }, { value: "", label: "All" }]}
        />
      </PageHeader>
      {q.isLoading ? <Card><SkeletonRows rows={4} /></Card> : q.error ? <ErrorBox error={q.error} /> : !q.data!.items.length ? (
        <Card><Empty icon={<ShieldCheck />} title={status === "pending" ? "Nothing waits for a decision" : "No approvals"} text={status === "pending" ? "When a workflow needs a human, it appears here and you get a notification." : undefined} /></Card>
      ) : (
        <div className="flex flex-col gap-3">{q.data!.items.map((a) => <ApprovalCard key={a.id} a={a} />)}</div>
      )}
    </Page>
  );
}

function ApprovalCard({ a }: { a: Approval }) {
  const qc = useQueryClient();
  const { me, can } = usePerms();
  const flash = useFlash(a.id);
  const [comment, setComment] = useState("");
  const decide = useMutation({
    mutationFn: (approved: boolean) => unwrap(api().POST("/approvals/{id}/decision", { params: { path: { id: a.id } }, body: { approved, comment } })),
    onSuccess: (_, approved) => { qc.invalidateQueries({ queryKey: ["approvals"] }); toast.success(approved ? "Approved" : "Rejected", { description: `${a.workflow_name ?? "Workflow"} continues.` }); },
  });
  const pending = a.status === "pending";
  const mine = !!me && a.approvers.some((x) => x === me.name || (me.roles ?? []).includes(x as never));
  const canDecide = can("approvals:decide") && mine;
  const overdue = pending && new Date(a.due_at).getTime() < Date.now();
  const soon = pending && !overdue && new Date(a.due_at).getTime() - Date.now() < 3600_000;
  const policy = !!a.action;
  return (
    <Card className={cn("relative overflow-hidden", pending && "border-line-strong")}>
      {flash && <span key={flash} className="pointer-events-none absolute inset-0 animate-flash" aria-hidden />}
      {pending && <span className={cn("absolute inset-y-0 left-0 w-[3px]", policy ? "bg-serious" : "bg-warn")} aria-hidden />}
      <div className="flex flex-col gap-3 p-4 pl-5">
        <div className="flex flex-wrap items-center gap-2">
          <StatusBadge status={a.status} />
          {policy && <Badge tone="orange"><ShieldAlert /> Policy hold · {a.action}</Badge>}
          <span className="text-sm text-fg-2">{a.workflow_name ?? a.workflow_id} · <code className="text-xs">{a.node_id}</code></span>
          <Link className="inline-flex items-center gap-1 text-xs text-accent hover:underline" to={`/executions/${a.execution_id}`}>execution <ExternalLink className="size-3" /></Link>
          <span className={cn("ml-auto inline-flex items-center gap-1 text-xs", overdue ? "font-medium text-crit" : soon ? "font-medium text-warn" : "text-fg-3")}>
            {pending ? <><AlarmClock className="size-3.5" /> {overdue ? "overdue" : <>due <Time iso={a.due_at} /></>}</> : <>{a.decided_by ?? "system"} · <Time iso={a.decided_at ?? a.updated_at} /></>}
          </span>
        </div>
        {a.message && <p className="text-[15px] leading-6">{a.message}</p>}
        {a.command && Object.keys(a.command).length > 0 && (
          <details className="group">
            <summary className="cursor-pointer list-none text-xs text-fg-3 hover:text-fg"><span className="group-open:hidden">Show what will run</span><span className="hidden group-open:inline">Hide</span></summary>
            <JsonView value={a.command} className="mt-2" />
          </details>
        )}
        <div className="flex items-center gap-1.5 text-xs text-fg-3"><Users className="size-3.5" /> {a.approvers.join(", ")}</div>
        {a.comment && <div className="rounded-md bg-hover/60 px-3 py-2 text-sm text-fg-2">“{a.comment}”</div>}
        {pending && (
          <div className="flex flex-wrap items-center gap-2 border-t border-line pt-3">
            <Input placeholder="Comment (optional, kept in the audit log)" value={comment} onChange={(e) => setComment(e.target.value)} className="min-w-[220px] flex-1" disabled={!canDecide} />
            <Button variant="danger-ghost" onClick={() => decide.mutate(false)} loading={decide.isPending && decide.variables === false} disabled={!canDecide || decide.isPending} tip={canDecide ? undefined : can("approvals:decide") ? "You are not one of the approvers" : requires("approvals:decide")}><X /> Reject</Button>
            <Button variant="primary" onClick={() => decide.mutate(true)} loading={decide.isPending && decide.variables === true} disabled={!canDecide || decide.isPending} tip={canDecide ? undefined : can("approvals:decide") ? "You are not one of the approvers" : requires("approvals:decide")}><Check /> Approve</Button>
          </div>
        )}
        <ErrorBox error={decide.error} />
      </div>
    </Card>
  );
}
