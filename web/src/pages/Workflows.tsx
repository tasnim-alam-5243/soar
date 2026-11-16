import { useMemo, useState } from "react";
import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router-dom";
import { Bell, CalendarClock, GitBranch, Hand, Plus, Radio, Search, Webhook } from "lucide-react";
import { api, unwrap, type Workflow } from "@/lib/api";
import { useHotkeys } from "@/lib/hotkeys";
import { requires, usePerms } from "@/lib/perms";
import { Page } from "@/components/Layout";
import { Empty, ErrorBox, PageHeader, SkeletonRows, StatusBadge, Time } from "@/components/common";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Dialog, DialogContent } from "@/components/ui/dialog";
import { FormField, Input, Segmented } from "@/components/ui/input";

const starter = {
  inputs: [{ name: "ip", from: 'alert.observables.exists(o, o.type == "ip") ? alert.observables.filter(o, o.type == "ip")[0].value : ""' }],
  nodes: [
    { id: "log", action: "notify.log", params: { message: '"triage of " + inputs.ip' } },
    { id: "open_case", action: "case.create", params: { title: '"Investigate " + inputs.ip', severity: 3 } },
  ],
  edges: ["log -> open_case"],
};

const triggerIcon: Record<string, React.ElementType> = { alert: Bell, schedule: CalendarClock, cron: CalendarClock, webhook: Webhook, event: Radio, manual: Hand };

export default function Workflows() {
  const { can } = usePerms();
  const [status, setStatus] = useState("");
  const [q, setQ] = useState("");
  const [creating, setCreating] = useState(false);
  const list = useInfiniteQuery({
    queryKey: ["workflows", status],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) => unwrap(api().GET("/workflows", { params: { query: { limit: 200, sort: "name", ...(status ? { status } : {}), ...(pageParam ? { cursor: pageParam } : {}) } } })),
    getNextPageParam: (l) => l.next_cursor || undefined,
  });
  useHotkeys({ n: () => can("workflows:write") && setCreating(true) }, [can]);
  // one row per workflow name, its versions folded under the newest
  const groups = useMemo(() => {
    const all = (list.data?.pages.flatMap((p) => p.items) ?? []).filter((w) => !q || w.name.toLowerCase().includes(q.toLowerCase()) || (w.description ?? "").toLowerCase().includes(q.toLowerCase()));
    const by = new Map<string, Workflow[]>();
    for (const w of all) by.set(w.name, [...(by.get(w.name) ?? []), w]);
    return [...by.values()].map((vs) => vs.sort((a, b) => b.version - a.version));
  }, [list.data, q]);

  return (
    <Page>
      <PageHeader
        title="Workflows"
        subtitle="Playbooks. Published versions are immutable; every execution pins the version it ran."
        actions={<Button variant="primary" onClick={() => setCreating(true)} disabled={!can("workflows:write")} tip={can("workflows:write") ? "New workflow (n)" : requires("workflows:write")}><Plus /> New workflow</Button>}
      >
        <div className="flex flex-wrap items-center gap-2">
          <Segmented value={status} onChange={setStatus} options={[{ value: "", label: "All" }, { value: "published", label: "Published" }, { value: "draft", label: "Drafts" }, { value: "archived", label: "Archived" }]} />
          <Input icon={<Search />} placeholder="Filter by name" value={q} onChange={(e) => setQ(e.target.value)} className="w-64" />
        </div>
      </PageHeader>
      <Card className="overflow-hidden">
        <div className="grid grid-cols-[minmax(260px,1fr)_110px_minmax(160px,240px)_120px_140px] items-center gap-3 border-b border-line px-4 py-2 text-2xs font-medium uppercase tracking-[0.05em] text-fg-3">
          <span>Workflow</span><span>Status</span><span>Triggers</span><span>Versions</span><span className="text-right">Updated</span>
        </div>
        {list.isLoading ? <SkeletonRows rows={6} /> : list.error ? <div className="p-4"><ErrorBox error={list.error} /></div> : !groups.length ? (
          <Empty icon={<GitBranch />} title="No workflows" text="Create a workflow to automate triage, enrichment and response." action={can("workflows:write") && <Button variant="primary" onClick={() => setCreating(true)}><Plus /> New workflow</Button>} />
        ) : (
          groups.map((vs) => {
            const w = vs[0];
            const live = vs.find((v) => v.status === "published");
            return (
              <Link key={w.name} to={`/workflows/${(live ?? w).id}`} className="grid grid-cols-[minmax(260px,1fr)_110px_minmax(160px,240px)_120px_140px] items-center gap-3 border-b border-line px-4 py-3 last:border-0 hover:bg-hover/60">
                <div className="min-w-0">
                  <div className="flex items-center gap-2">
                    <GitBranch className="size-4 shrink-0 text-fg-3" />
                    <span className="truncate font-medium">{w.name}</span>
                  </div>
                  {w.description && <div className="ml-6 truncate text-xs text-fg-3">{w.description}</div>}
                </div>
                <StatusBadge status={(live ?? w).status} />
                <div className="flex min-w-0 flex-wrap gap-1">
                  {(w.triggers ?? []).length ? (w.triggers ?? []).map((t, i) => {
                    const Icon = triggerIcon[t.type] ?? Radio;
                    return <Badge key={i} title={(t as { match?: string }).match ?? t.cron}><Icon />{t.type}{t.cron ? ` ${t.cron}` : ""}</Badge>;
                  }) : <Badge tone="outline"><Hand />manual</Badge>}
                </div>
                <span className="text-sm text-fg-2">
                  v{w.version}
                  {live && live.id !== w.id && <span className="text-xs text-fg-3"> · v{live.version} live</span>}
                  {vs.length > 1 && <span className="text-xs text-fg-4"> · {vs.length} total</span>}
                </span>
                <span className="text-right text-xs text-fg-3"><Time iso={w.updated_at ?? w.created_at} />{w.created_by && <span className="block text-fg-4">{w.created_by}</span>}</span>
              </Link>
            );
          })
        )}
      </Card>
      {creating && <NewWorkflow onClose={() => setCreating(false)} />}
    </Page>
  );
}

function NewWorkflow({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient();
  const nav = useNavigate();
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const valid = /^[a-z][a-z0-9_]{1,62}$/.test(name);
  const m = useMutation({
    mutationFn: () => unwrap(api().POST("/workflows", { body: { name, description, definition: starter } })),
    onSuccess: (w) => { qc.invalidateQueries({ queryKey: ["workflows"] }); nav(`/workflows/${w.id}`); },
  });
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent
        title="New workflow"
        description="Starts as a draft with a two-step example you can replace."
        footer={<><Button variant="ghost" onClick={onClose}>Cancel</Button><Button variant="primary" type="submit" form="new-wf" disabled={!name} loading={m.isPending}>Create draft</Button></>}
      >
        <form id="new-wf" className="flex flex-col gap-4" onSubmit={(e) => { e.preventDefault(); m.mutate(); }}>
          <FormField label="Name" hint="Lower case, digits and underscores (phishing_triage)." error={name && !valid ? "Use a-z, 0-9 and _, starting with a letter" : undefined}>
            <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="phishing_triage" autoFocus className="font-mono" />
          </FormField>
          <FormField label="Description"><Input value={description} onChange={(e) => setDescription(e.target.value)} placeholder="What it does and when it runs" /></FormField>
          <ErrorBox error={m.error} />
        </form>
      </DialogContent>
    </Dialog>
  );
}
