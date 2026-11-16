import { useEffect, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams } from "react-router-dom";
import { toast } from "sonner";
import {
  AlarmClock, ArrowLeft, Bell, Briefcase, CheckCircle2, Circle, CircleDot, Download, ExternalLink, FileText, History, ListChecks, Paperclip, Plus,
  Tag, Upload, X,
} from "lucide-react";
import { api, raw, unwrap, type Case } from "@/lib/api";
import { absolute, bytes, humanize } from "@/lib/format";
import { requires, usePerms } from "@/lib/perms";
import { VERDICTS, verdictName } from "@/lib/severity";
import { cn } from "@/lib/utils";
import { Page } from "@/components/Layout";
import { AssigneePicker } from "@/components/AssigneePicker";
import { Avatar, CopyButton, Empty, ErrorBox, JsonView, ObservableChip, SeverityBadge, SeverityGlyph, SkeletonRows, StatusBadge, Time } from "@/components/common";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card, Panel } from "@/components/ui/card";
import { Dialog, DialogContent } from "@/components/ui/dialog";
import { Input, Select, Textarea } from "@/components/ui/input";
import { Menu, MenuContent, MenuItem, MenuTrigger } from "@/components/ui/menu";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { slaState } from "./Cases";

type CasePatchBody = Partial<Pick<Case, "status" | "assignee" | "verdict" | "severity" | "summary" | "title" | "tags">>;

export default function CaseDetail() {
  const { id = "" } = useParams();
  const qc = useQueryClient();
  const { can } = usePerms();
  const canWrite = can("cases:write");
  const q = useQuery({ queryKey: ["case", id], queryFn: () => unwrap(api().GET("/cases/{id}", { params: { path: { id } } })) });
  const patch = useMutation({
    mutationFn: (body: CasePatchBody) => unwrap(api().PATCH("/cases/{id}", { params: { path: { id } }, body: body as never })),
    onSuccess: (c, body) => {
      qc.setQueryData(["case", id], c);
      qc.invalidateQueries({ queryKey: ["timeline", id] });
      qc.invalidateQueries({ queryKey: ["cases"] });
      if (body.status === "closed") {
        qc.invalidateQueries({ queryKey: ["alerts"] });
        toast.success("Case closed", { description: (c.alert_ids?.length ?? 0) > 0 ? "Its open alerts were closed with the case verdict." : undefined });
      }
    },
    onError: (e) => toast.error("Could not update the case", { description: e instanceof Error ? e.message : String(e) }),
  });
  const [closing, setClosing] = useState(false);

  if (q.isLoading) return <Page><SkeletonRows rows={8} /></Page>;
  if (q.error) return <Page><ErrorBox error={q.error} /></Page>;
  const c = q.data!;
  const sla = slaState(c);
  const no = canWrite ? undefined : requires("cases:write");

  return (
    <Page>
      <div className="mb-4 flex items-center gap-2 text-sm text-fg-3">
        <Link to="/cases" className="inline-flex items-center gap-1 hover:text-fg"><ArrowLeft className="size-3.5" /> Cases</Link>
        <span className="text-fg-4">/</span>
        <span className="font-mono text-xs">{c.id}</span>
        <CopyButton value={c.id} label="Copy id" />
      </div>
      <div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_300px]">
        <div className="min-w-0">
          <div className="mb-5 flex flex-col gap-3">
            <div className="flex flex-wrap items-center gap-3">
              <SeverityBadge severity={c.severity} />
              <StatusBadge status={c.status} />
              {c.verdict && <Badge tone="outline">{verdictName[c.verdict]}</Badge>}
              {sla.due && c.status !== "closed" && (
                <span className={cn("inline-flex items-center gap-1 text-xs font-medium", sla.breached ? "text-crit" : sla.soon ? "text-warn" : "text-fg-3")}>
                  <AlarmClock className="size-3.5" /> {sla.breached ? "SLA breached" : <>SLA due <Time iso={sla.due} /></>}
                </span>
              )}
            </div>
            <EditableTitle value={c.title} disabled={!canWrite} onSave={(title) => patch.mutate({ title })} />
            <div className="text-sm text-fg-3">
              Opened <Time iso={c.created_at} />
              {c.closed_at && <> · closed <Time iso={c.closed_at} /></>}
            </div>
          </div>
          <Summary value={c.summary ?? ""} disabled={!canWrite} onSave={(summary) => patch.mutate({ summary })} />
          <Tabs defaultValue="alerts" className="mt-6">
            <TabsList>
              <TabsTrigger value="alerts" count={c.alert_ids?.length ?? 0}><Bell className="size-3.5" /> Alerts</TabsTrigger>
              <TabsTrigger value="observables"><Tag className="size-3.5" /> Observables</TabsTrigger>
              <TabsTrigger value="tasks"><ListChecks className="size-3.5" /> Tasks</TabsTrigger>
              <TabsTrigger value="evidence" count={c.evidence?.length ?? 0}><Paperclip className="size-3.5" /> Evidence</TabsTrigger>
              <TabsTrigger value="timeline"><History className="size-3.5" /> Timeline</TabsTrigger>
            </TabsList>
            <TabsContent value="alerts"><CaseAlerts id={id} /></TabsContent>
            <TabsContent value="observables"><CaseObservables id={id} canWrite={canWrite} /></TabsContent>
            <TabsContent value="tasks"><Tasks id={id} canWrite={canWrite} /></TabsContent>
            <TabsContent value="evidence"><Evidence c={c} canWrite={canWrite} /></TabsContent>
            <TabsContent value="timeline"><Timeline id={id} /></TabsContent>
          </Tabs>
        </div>

        <aside className="flex flex-col gap-4">
          <Card className="p-4">
            <div className="flex flex-col gap-3">
              {c.status !== "closed" ? (
                <Button variant="primary" onClick={() => setClosing(true)} disabled={!canWrite} tip={no}><CheckCircle2 /> Close case</Button>
              ) : (
                <Button onClick={() => patch.mutate({ status: "open" })} disabled={!canWrite} tip={no}>Reopen case</Button>
              )}
              <div className="flex flex-col divide-y divide-line">
                <Prop label="Status">
                  <Menu>
                    <MenuTrigger asChild disabled={!canWrite}><button className="rounded px-1 py-0.5 hover:bg-hover disabled:hover:bg-transparent"><StatusBadge status={c.status} /></button></MenuTrigger>
                    <MenuContent>
                      {(["open", "in_progress", "closed"] as const).map((s) => (
                        <MenuItem key={s} onSelect={() => (s === "closed" ? setClosing(true) : patch.mutate({ status: s }))} icon={s === "closed" ? <CheckCircle2 /> : <CircleDot />}>{humanize(s)}</MenuItem>
                      ))}
                    </MenuContent>
                  </Menu>
                </Prop>
                <Prop label="Severity">
                  <Menu>
                    <MenuTrigger asChild disabled={!canWrite}><button className="rounded px-1 py-0.5 hover:bg-hover disabled:hover:bg-transparent"><SeverityBadge severity={c.severity} /></button></MenuTrigger>
                    <MenuContent>
                      {[5, 4, 3, 2, 1].map((s) => <MenuItem key={s} onSelect={() => patch.mutate({ severity: s })}><SeverityBadge severity={s} /></MenuItem>)}
                    </MenuContent>
                  </Menu>
                </Prop>
                <Prop label="Assignee">
                  <AssigneePicker value={c.assignee} onChange={(assignee) => patch.mutate({ assignee })} disabled={!canWrite}>
                    <button className="flex items-center gap-1.5 rounded px-1 py-0.5 text-sm hover:bg-hover" disabled={!canWrite}>
                      <Avatar name={c.assignee} size={18} /> {c.assignee || <span className="text-fg-4">Unassigned</span>}
                    </button>
                  </AssigneePicker>
                </Prop>
                <Prop label="Verdict">
                  <Select value={c.verdict ?? ""} onChange={(e) => patch.mutate({ verdict: e.target.value as Case["verdict"] })} disabled={!canWrite} className="w-40">
                    <option value="">Not set</option>
                    {VERDICTS.map((v) => <option key={v} value={v}>{verdictName[v]}</option>)}
                  </Select>
                </Prop>
                <Prop label="SLA">
                  {sla.due ? <span className={cn("text-sm", sla.breached && "text-crit")}>{absolute(sla.due, undefined, false)}</span> : <span className="text-sm text-fg-4">None</span>}
                </Prop>
                <Prop label="Updated"><span className="text-sm text-fg-2"><Time iso={c.updated_at} /></span></Prop>
              </div>
            </div>
          </Card>
          <Tags c={c} disabled={!canWrite} onSave={(tags) => patch.mutate({ tags })} />
          {(c.external_refs?.length ?? 0) > 0 && (
            <Panel title="Linked tickets" bodyClassName="flex flex-col gap-2 p-3">
              {c.external_refs!.map((r) => (
                <div key={r.system + r.id} className="flex items-center gap-2 text-sm">
                  <Badge>{r.system}</Badge>
                  {r.url && /^https?:\/\//.test(r.url) ? <a href={r.url} target="_blank" rel="noreferrer noopener" className="inline-flex items-center gap-1 font-mono text-xs text-accent hover:underline">#{r.id} <ExternalLink className="size-3" /></a> : <span className="font-mono text-xs">#{r.id}</span>}
                  {r.last_synced_at && <span className="ml-auto text-2xs text-fg-4">synced <Time iso={r.last_synced_at} /></span>}
                </div>
              ))}
            </Panel>
          )}
        </aside>
      </div>
      {closing && (
        <CloseCaseDialog
          c={c}
          busy={patch.isPending}
          onClose={() => setClosing(false)}
          onConfirm={(verdict) => patch.mutate({ status: "closed", verdict: verdict as Case["verdict"] }, { onSuccess: () => setClosing(false) })}
        />
      )}
    </Page>
  );
}

function Prop({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex min-h-10 items-center justify-between gap-3 py-1.5">
      <span className="text-xs text-fg-3">{label}</span>
      <div className="flex min-w-0 justify-end">{children}</div>
    </div>
  );
}

function EditableTitle({ value, onSave, disabled }: { value: string; onSave: (v: string) => void; disabled?: boolean }) {
  const [v, setV] = useState(value);
  useEffect(() => setV(value), [value]);
  return (
    <input
      value={v}
      disabled={disabled}
      onChange={(e) => setV(e.target.value)}
      onBlur={() => v.trim() && v !== value && onSave(v.trim())}
      onKeyDown={(e) => { if (e.key === "Enter") e.currentTarget.blur(); if (e.key === "Escape") { setV(value); e.currentTarget.blur(); } }}
      className="-mx-1.5 w-full rounded-md bg-transparent px-1.5 py-0.5 text-xl font-semibold tracking-[-0.01em] hover:bg-hover/60 focus:bg-hover focus:outline-none disabled:hover:bg-transparent"
      aria-label="Case title"
    />
  );
}

function Summary({ value, onSave, disabled }: { value: string; onSave: (v: string) => void; disabled?: boolean }) {
  const [editing, setEditing] = useState(false);
  const [v, setV] = useState(value);
  useEffect(() => setV(value), [value]);
  if (editing) {
    return (
      <div className="flex flex-col gap-2">
        <Textarea autoFocus className="min-h-[120px] font-sans text-sm leading-6" value={v} onChange={(e) => setV(e.target.value)} placeholder="What happened, what is known, what was done…" />
        <div className="flex gap-2">
          <Button size="sm" variant="primary" onClick={() => { onSave(v); setEditing(false); }}>Save</Button>
          <Button size="sm" variant="ghost" onClick={() => { setV(value); setEditing(false); }}>Cancel</Button>
        </div>
      </div>
    );
  }
  return (
    <button
      disabled={disabled}
      onClick={() => setEditing(true)}
      className="-mx-2 block w-[calc(100%+16px)] rounded-md px-2 py-1.5 text-left text-sm leading-6 text-fg-2 hover:bg-hover/60 disabled:hover:bg-transparent"
    >
      {value ? <span className="whitespace-pre-wrap">{value}</span> : <span className="text-fg-4">{disabled ? "No summary." : "Add a summary…"}</span>}
    </button>
  );
}

function Tags({ c, onSave, disabled }: { c: Case; onSave: (t: string[]) => void; disabled?: boolean }) {
  const [t, setT] = useState("");
  const tags = c.tags ?? [];
  return (
    <Panel title="Tags" bodyClassName="p-3">
      <div className="flex flex-wrap gap-1.5">
        {tags.map((x) => (
          <Badge key={x} className="pr-0.5">
            {x}
            {!disabled && <button className="rounded p-0.5 hover:bg-line" onClick={() => onSave(tags.filter((y) => y !== x))} aria-label={`Remove tag ${x}`}><X className="size-3" /></button>}
          </Badge>
        ))}
        {!tags.length && <span className="text-xs text-fg-4">No tags</span>}
      </div>
      {!disabled && (
        <form className="mt-2" onSubmit={(e) => { e.preventDefault(); const n = t.trim(); if (n && !tags.includes(n)) onSave([...tags, n]); setT(""); }}>
          <Input value={t} onChange={(e) => setT(e.target.value)} placeholder="Add a tag and press Enter" className="h-7 text-xs" />
        </form>
      )}
    </Panel>
  );
}

function CloseCaseDialog({ c, onClose, onConfirm, busy }: { c: Case; onClose: () => void; onConfirm: (v: string) => void; busy: boolean }) {
  const [verdict, setVerdict] = useState(c.verdict || "true_positive");
  const n = c.alert_ids?.length ?? 0;
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent
        title="Close case"
        description={n > 0 ? `${n} linked alert${n === 1 ? "" : "s"} that are still open will be closed with this verdict.` : "Record the outcome of the investigation."}
        footer={<><Button variant="ghost" onClick={onClose}>Cancel</Button><Button variant="primary" loading={busy} onClick={() => onConfirm(verdict)}><CheckCircle2 /> Close case</Button></>}
      >
        <div role="radiogroup" className="grid grid-cols-2 gap-2">
          {VERDICTS.map((v) => (
            <button key={v} type="button" role="radio" aria-checked={verdict === v} onClick={() => setVerdict(v)}
              className={cn("rounded-lg border px-3 py-2.5 text-left text-sm font-medium transition-colors", verdict === v ? "border-accent bg-accent/10" : "border-line-strong hover:bg-hover")}>
              {verdictName[v]}
            </button>
          ))}
        </div>
      </DialogContent>
    </Dialog>
  );
}

function CaseAlerts({ id }: { id: string }) {
  const q = useQuery({ queryKey: ["case-alerts", id], queryFn: () => unwrap(api().GET("/cases/{id}/alerts", { params: { path: { id }, query: { limit: 200 } } })) });
  if (q.isLoading) return <SkeletonRows rows={3} />;
  if (q.error) return <ErrorBox error={q.error} />;
  if (!q.data!.items.length) return <Card><Empty icon={<Bell />} title="No linked alerts" text="Link alerts from the Alerts page with the Case action." /></Card>;
  return (
    <Card className="divide-y divide-line">
      {q.data!.items.map((ca) => {
        const a = ca.snapshot as { title?: string; severity?: number; source_id?: string; received_at?: string; status?: string; occurrence_count?: number; observables?: { type: string; value: string; role?: string }[] };
        return (
          <div key={ca.id} className="flex flex-col gap-2 px-4 py-3">
            <div className="flex items-center gap-3">
              <SeverityGlyph severity={a.severity} />
              <Link to={`/alerts?alert=${ca.alert_id}`} className="min-w-0 flex-1 truncate text-sm font-medium hover:underline">{a.title}</Link>
              <span className="font-mono text-2xs text-fg-4">{a.source_id}</span>
              <span className="text-xs text-fg-3"><Time iso={a.received_at} /></span>
            </div>
            {(a.observables?.length ?? 0) > 0 && <div className="flex flex-wrap gap-1.5 pl-[27px]">{a.observables!.slice(0, 8).map((o, i) => <ObservableChip key={i} type={o.type} value={o.value} role={o.role} />)}</div>}
            <div className="pl-[27px] text-2xs text-fg-4">Linked <Time iso={ca.linked_at} /> · snapshot kept after the alert expires</div>
          </div>
        );
      })}
    </Card>
  );
}

const OBS_TYPES = ["ip", "domain", "url", "hash", "email", "host", "user", "file_path"];

function CaseObservables({ id, canWrite }: { id: string; canWrite: boolean }) {
  const qc = useQueryClient();
  const [type, setType] = useState("ip");
  const [value, setValue] = useState("");
  const q = useQuery({ queryKey: ["observables", "case", id], queryFn: () => unwrap(api().GET("/observables", { params: { query: { case_id: id, limit: 200 } as never } })) });
  const add = useMutation({
    mutationFn: () => unwrap(api().POST("/cases/{id}/observables", { params: { path: { id } }, body: { type: type as never, value } })),
    onSuccess: () => { setValue(""); qc.invalidateQueries({ queryKey: ["observables", "case", id] }); qc.invalidateQueries({ queryKey: ["timeline", id] }); },
  });
  const items = q.data?.items ?? [];
  const groups = OBS_TYPES.map((t) => [t, items.filter((o) => o.type === t)] as const).filter(([, l]) => l.length);
  return (
    <Card className="p-4">
      {canWrite && (
        <form className="mb-4 flex gap-2" onSubmit={(e) => { e.preventDefault(); if (value.trim()) add.mutate(); }}>
          <Select value={type} onChange={(e) => setType(e.target.value)} className="w-32">{OBS_TYPES.map((t) => <option key={t} value={t}>{t}</option>)}</Select>
          <Input value={value} onChange={(e) => setValue(e.target.value)} placeholder="203.0.113.7, evil.example, a file hash…" className="flex-1 font-mono text-xs" />
          <Button type="submit" disabled={!value.trim()} loading={add.isPending}><Plus /> Add</Button>
        </form>
      )}
      <ErrorBox error={add.error} className="mb-3" />
      {q.isLoading ? <SkeletonRows rows={2} /> : !items.length ? <Empty icon={<Tag />} title="No observables" text="Indicators from linked alerts appear here; add more by hand." className="py-8" /> : (
        <div className="flex flex-col gap-4">
          {groups.map(([t, list]) => (
            <div key={t} className="flex flex-col gap-1.5">
              <div className="label">{t} · {list.length}</div>
              <div className="flex flex-wrap gap-1.5">
                {list.map((o) => <ObservableChip key={o.id} type={o.type} value={o.value} role={o.reputation ? String((o.reputation as { verdict?: string }).verdict ?? "") : undefined} />)}
              </div>
            </div>
          ))}
        </div>
      )}
    </Card>
  );
}

function Tasks({ id, canWrite }: { id: string; canWrite: boolean }) {
  const qc = useQueryClient();
  const [title, setTitle] = useState("");
  const q = useQuery({ queryKey: ["tasks", id], queryFn: () => unwrap(api().GET("/cases/{id}/tasks", { params: { path: { id }, query: { limit: 200 } } })) });
  const add = useMutation({
    mutationFn: () => unwrap(api().POST("/cases/{id}/tasks", { params: { path: { id } }, body: { title } })),
    onSuccess: () => { setTitle(""); qc.invalidateQueries({ queryKey: ["tasks", id] }); },
  });
  const update = useMutation({
    mutationFn: (t: { id: string; status?: string; assignee?: string }) =>
      unwrap(api().PATCH("/cases/{id}/tasks/{task_id}", { params: { path: { id, task_id: t.id } }, body: { ...(t.status ? { status: t.status } : {}), ...(t.assignee !== undefined ? { assignee: t.assignee } : {}) } as never })),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["tasks", id] }),
  });
  const items = q.data?.items ?? [];
  const done = items.filter((t) => t.status === "done").length;
  return (
    <Card>
      {items.length > 0 && (
        <div className="flex items-center gap-3 border-b border-line px-4 py-2.5">
          <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-good/15"><div className="h-full rounded-full bg-good" style={{ width: `${(done / items.length) * 100}%` }} /></div>
          <span className="tabular text-xs text-fg-3">{done} of {items.length} done</span>
        </div>
      )}
      <div className="divide-y divide-line">
        {items.map((t) => (
          <div key={t.id} className="flex items-center gap-3 px-4 py-2.5">
            <button
              disabled={!canWrite}
              onClick={() => update.mutate({ id: t.id, status: t.status === "done" ? "open" : "done" })}
              className="text-fg-3 hover:text-good disabled:hover:text-fg-3"
              aria-label={t.status === "done" ? "Mark as open" : "Mark as done"}
            >
              {t.status === "done" ? <CheckCircle2 className="size-[18px] text-good" /> : <Circle className="size-[18px]" />}
            </button>
            <span className={cn("min-w-0 flex-1 truncate text-sm", t.status === "done" && "text-fg-3 line-through", t.status === "cancelled" && "text-fg-4 line-through")}>{t.title}</span>
            {t.status === "in_progress" && <StatusBadge status="in_progress" />}
            {t.due_at && <span className={cn("text-xs", t.status !== "done" && new Date(t.due_at) < new Date() ? "text-crit" : "text-fg-3")}>due <Time iso={t.due_at} /></span>}
            <AssigneePicker value={t.assignee} onChange={(assignee) => update.mutate({ id: t.id, assignee })} disabled={!canWrite}>
              <button className="rounded-full" disabled={!canWrite} aria-label="Assign task"><Avatar name={t.assignee} size={22} /></button>
            </AssigneePicker>
          </div>
        ))}
      </div>
      {!items.length && !q.isLoading && <Empty icon={<ListChecks />} title="No tasks" text="Break the investigation into steps others can pick up." className="py-8" />}
      {canWrite && (
        <form className="flex gap-2 border-t border-line p-3" onSubmit={(e) => { e.preventDefault(); if (title.trim()) add.mutate(); }}>
          <Input value={title} onChange={(e) => setTitle(e.target.value)} placeholder="Add a task…" className="flex-1" />
          <Button type="submit" disabled={!title.trim()} loading={add.isPending}><Plus /> Add</Button>
        </form>
      )}
      <ErrorBox error={add.error ?? update.error} className="m-3" />
    </Card>
  );
}

function Evidence({ c, canWrite }: { c: Case; canWrite: boolean }) {
  const qc = useQueryClient();
  const file = useRef<HTMLInputElement>(null);
  const [drag, setDrag] = useState(false);
  const upload = useMutation({
    mutationFn: async (f: File) => raw(`/cases/${c.id}/evidence?filename=${encodeURIComponent(f.name)}`, { method: "POST", body: f, headers: { "Content-Type": f.type || "application/octet-stream" } }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["case", c.id] }); toast.success("Evidence stored"); },
  });
  const download = async (eid: string) => {
    const link = await unwrap(api().GET("/cases/{id}/evidence/{eid}", { params: { path: { id: c.id, eid } } }));
    window.open(link.url, "_blank", "noopener");
  };
  const pick = (files: FileList | null) => files?.[0] && upload.mutate(files[0]);
  return (
    <Card className="p-4">
      {canWrite && (
        <div
          onDragOver={(e) => { e.preventDefault(); setDrag(true); }}
          onDragLeave={() => setDrag(false)}
          onDrop={(e) => { e.preventDefault(); setDrag(false); pick(e.dataTransfer.files); }}
          className={cn("mb-4 flex flex-col items-center gap-2 rounded-lg border border-dashed px-4 py-6 text-center transition-colors", drag ? "border-accent bg-accent/5" : "border-line-strong")}
        >
          <Upload className="size-5 text-fg-3" />
          <div className="text-sm text-fg-2">Drop a file here or <button className="font-medium text-accent hover:underline" onClick={() => file.current?.click()}>browse</button></div>
          <div className="text-xs text-fg-4">Up to 25 MB. Stored with its SHA-256; links expire.</div>
          <input ref={file} type="file" className="hidden" onChange={(e) => pick(e.target.files)} />
          {upload.isPending && <div className="text-xs text-fg-3">Uploading…</div>}
        </div>
      )}
      <ErrorBox error={upload.error} className="mb-3" />
      {!c.evidence?.length ? <Empty icon={<FileText />} title="No evidence" text="Attach mail headers, memory captures, screenshots or exports." className="py-6" /> : (
        <div className="divide-y divide-line rounded-md border border-line">
          {c.evidence.map((e) => (
            <div key={e.id} className="flex items-center gap-3 px-3 py-2.5">
              <FileText className="size-4 text-fg-3" />
              <div className="min-w-0 flex-1">
                <div className="truncate text-sm font-medium">{e.name}</div>
                <div className="flex items-center gap-1 text-2xs text-fg-3">
                  {bytes(e.size)} · <span className="font-mono">{e.sha256.slice(0, 16)}…</span><CopyButton value={e.sha256} label="Copy SHA-256" className="p-0.5" /> · {e.added_by} · <Time iso={e.added_at} />
                </div>
              </div>
              <Button size="icon-sm" variant="ghost" onClick={() => download(e.id)} tip="Download" aria-label={`Download ${e.name}`}><Download /></Button>
            </div>
          ))}
        </div>
      )}
    </Card>
  );
}

const eventIcon: Record<string, React.ElementType> = {
  "case.created": Briefcase,
  "case.updated": CircleDot,
  "alerts.linked": Bell,
  "alerts.closed": CheckCircle2,
  "task.created": ListChecks,
  "observable.added": Tag,
  "evidence.added": Paperclip,
};

function Timeline({ id }: { id: string }) {
  const q = useQuery({ queryKey: ["timeline", id], queryFn: () => unwrap(api().GET("/cases/{id}/timeline", { params: { path: { id }, query: { limit: 200, sort: "-created_at" } } })) });
  if (q.isLoading) return <SkeletonRows rows={4} />;
  if (q.error) return <ErrorBox error={q.error} />;
  const items = q.data?.items ?? [];
  if (!items.length) return <Card><Empty icon={<History />} title="Nothing yet" /></Card>;
  return (
    <Card className="p-4">
      <ol className="relative flex flex-col gap-0">
        {items.map((ev, i) => {
          const Icon = eventIcon[ev.event_type] ?? CircleDot;
          const data = (ev.data ?? {}) as Record<string, unknown>;
          return (
            <li key={ev.id} className="relative flex gap-3 pb-5 last:pb-0">
              {i < items.length - 1 && <span className="absolute left-[11px] top-6 h-[calc(100%-18px)] w-px bg-line" aria-hidden />}
              <span className="relative z-[1] flex size-6 shrink-0 items-center justify-center rounded-full border border-line bg-raised"><Icon className="size-3.5 text-fg-3" /></span>
              <div className="min-w-0 flex-1 pt-0.5">
                <div className="flex flex-wrap items-baseline gap-x-2 text-sm">
                  <span className="font-medium">{ev.actor || "system"}</span>
                  <span className="text-fg-2">{describe(ev.event_type, data)}</span>
                  <span className="text-xs text-fg-4"><Time iso={ev.created_at} /></span>
                </div>
                {Object.keys(data).length > 0 && !["case.updated", "case.created"].includes(ev.event_type) && (
                  <details className="mt-1">
                    <summary className="cursor-pointer text-2xs text-fg-4 hover:text-fg-3">details</summary>
                    <JsonView value={data} className="mt-1" />
                  </details>
                )}
              </div>
            </li>
          );
        })}
      </ol>
    </Card>
  );
}

function describe(type: string, d: Record<string, unknown>): string {
  switch (type) {
    case "case.created": return `opened the case${d.title ? ` “${d.title}”` : ""}`;
    case "case.updated": return `changed ${String(d.fields ?? "the case").replace(/,/g, ", ")}`;
    case "alerts.linked": return `linked ${String(d.alert_ids ?? "").split(",").filter(Boolean).length || "some"} alert(s)`;
    case "alerts.closed": return `closed ${String(d.alert_ids ?? "").split(",").filter(Boolean).length} linked alert(s)`;
    case "task.created": return `added the task “${d.title ?? ""}”`;
    case "observable.added": return `added ${d.type ?? "an observable"} ${d.value ?? ""}`;
    default: return humanize(type.replace(/\./g, " "));
  }
}

