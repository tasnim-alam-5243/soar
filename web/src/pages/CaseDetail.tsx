import { useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useParams } from "react-router-dom";
import { Download, Paperclip, Plus } from "lucide-react";
import { api, raw, unwrap, type Case } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input, Select, Textarea } from "@/components/ui/input";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Badge } from "@/components/ui/badge";
import { Empty, ErrorBox, Field, JsonView, Loading, PageHeader, SeverityBadge, StatusBadge } from "@/components/common";
import { ago, ts } from "@/lib/utils";

const verdicts = ["", "true_positive", "false_positive", "benign", "unknown"];

export default function CaseDetail() {
  const { id = "" } = useParams();
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["case", id], queryFn: () => unwrap(api().GET("/cases/{id}", { params: { path: { id } } })) });
  const patch = useMutation({
    mutationFn: (body: Partial<Pick<Case, "status" | "assignee" | "verdict" | "severity" | "summary">>) =>
      unwrap(api().PATCH("/cases/{id}", { params: { path: { id } }, body: body as never })),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["case", id] }); qc.invalidateQueries({ queryKey: ["timeline", id] }); },
  });
  if (q.isLoading) return <Loading />;
  if (q.error) return <ErrorBox error={q.error} />;
  const c = q.data!;
  return (
    <div>
      <PageHeader title={c.title} subtitle={`Opened ${ago(c.created_at)}${c.closed_at ? ` · closed ${ago(c.closed_at)}` : ""}`} />
      <div className="grid gap-4 lg:grid-cols-[1fr_320px]">
        <Tabs defaultValue="alerts">
          <TabsList>
            <TabsTrigger value="alerts">Alerts ({c.alert_ids?.length ?? 0})</TabsTrigger>
            <TabsTrigger value="observables">Observables</TabsTrigger>
            <TabsTrigger value="tasks">Tasks</TabsTrigger>
            <TabsTrigger value="evidence">Evidence ({c.evidence?.length ?? 0})</TabsTrigger>
            <TabsTrigger value="timeline">Timeline</TabsTrigger>
          </TabsList>
          <TabsContent value="alerts"><CaseAlerts id={id} /></TabsContent>
          <TabsContent value="observables"><CaseObservables id={id} /></TabsContent>
          <TabsContent value="tasks"><Tasks id={id} /></TabsContent>
          <TabsContent value="evidence"><Evidence c={c} /></TabsContent>
          <TabsContent value="timeline"><Timeline id={id} /></TabsContent>
        </Tabs>
        <Card>
          <CardHeader><CardTitle>Details</CardTitle></CardHeader>
          <CardContent className="flex flex-col gap-3">
            <Field label="Status">
              <Select value={c.status} onChange={(e) => patch.mutate({ status: e.target.value as Case["status"] })}>
                {["open", "in_progress", "closed"].map((s) => <option key={s}>{s}</option>)}
              </Select>
            </Field>
            <Field label="Severity">
              <Select value={c.severity} onChange={(e) => patch.mutate({ severity: Number(e.target.value) })}>
                {[1, 2, 3, 4, 5].map((s) => <option key={s} value={s}>{s}</option>)}
              </Select>
            </Field>
            <Field label="Verdict">
              <Select value={c.verdict ?? ""} onChange={(e) => patch.mutate({ verdict: e.target.value as Case["verdict"] })}>
                {verdicts.map((v) => <option key={v} value={v}>{v || "—"}</option>)}
              </Select>
            </Field>
            <Field label="Assignee">
              <Input defaultValue={c.assignee ?? ""} onBlur={(e) => e.target.value !== (c.assignee ?? "") && patch.mutate({ assignee: e.target.value })} />
            </Field>
            <Field label="Summary">
              <Textarea className="font-sans" defaultValue={c.summary ?? ""} onBlur={(e) => e.target.value !== (c.summary ?? "") && patch.mutate({ summary: e.target.value })} />
            </Field>
            <Field label="Current"><StatusBadge status={c.status} /> <SeverityBadge severity={c.severity} /></Field>
            {c.external_refs?.map((r) => <Field key={r.system} label={r.system}>{r.url ? <a className="text-primary hover:underline" href={r.url} target="_blank" rel="noreferrer">#{r.id}</a> : r.id}</Field>)}
            <ErrorBox error={patch.error} />
          </CardContent>
        </Card>
      </div>
    </div>
  );
}

function CaseAlerts({ id }: { id: string }) {
  const q = useQuery({ queryKey: ["case-alerts", id], queryFn: () => unwrap(api().GET("/cases/{id}/alerts", { params: { path: { id }, query: { limit: 200 } } })) });
  if (q.isLoading) return <Loading />;
  if (q.error) return <ErrorBox error={q.error} />;
  if (!q.data!.items.length) return <Card><Empty text="No alerts linked. Link alerts from the Alerts page." /></Card>;
  return (
    <div className="flex flex-col gap-2">
      {q.data!.items.map((ca) => {
        const a = ca.snapshot as { title?: string; severity?: number; source_id?: string; received_at?: string; observables?: { type: string; value: string }[] };
        return (
          <Card key={ca.id}>
            <CardContent className="pt-4">
              <div className="flex items-center gap-2">
                <SeverityBadge severity={a.severity} />
                <span className="font-medium">{a.title}</span>
                <span className="ml-auto text-xs text-muted-foreground">{a.source_id} · {ts(a.received_at)}</span>
              </div>
              <div className="mt-2 flex flex-wrap gap-1">{(a.observables ?? []).map((o, i) => <Badge key={i} tone="outline">{o.type}: {o.value}</Badge>)}</div>
            </CardContent>
          </Card>
        );
      })}
    </div>
  );
}

function CaseObservables({ id }: { id: string }) {
  const qc = useQueryClient();
  const [type, setType] = useState("ip");
  const [value, setValue] = useState("");
  const q = useQuery({ queryKey: ["observables", "case", id], queryFn: () => unwrap(api().GET("/observables", { params: { query: { case_id: id, limit: 200 } as never } })) });
  const add = useMutation({
    mutationFn: () => unwrap(api().POST("/cases/{id}/observables", { params: { path: { id } }, body: { type: type as never, value } })),
    onSuccess: () => { setValue(""); qc.invalidateQueries({ queryKey: ["observables", "case", id] }); },
  });
  return (
    <Card>
      <CardContent className="flex flex-col gap-3 pt-4">
        <form className="flex gap-2" onSubmit={(e) => { e.preventDefault(); add.mutate(); }}>
          <Select value={type} onChange={(e) => setType(e.target.value)} className="w-36">
            {["ip", "domain", "url", "hash", "email", "host", "user", "file_path"].map((t) => <option key={t}>{t}</option>)}
          </Select>
          <Input value={value} onChange={(e) => setValue(e.target.value)} placeholder="value" />
          <Button type="submit" disabled={!value}><Plus className="h-4 w-4" /> Add</Button>
        </form>
        <ErrorBox error={add.error} />
        {q.data?.items.length ? (
          <div className="flex flex-wrap gap-1.5">
            {q.data.items.map((o) => <Badge key={o.id} tone="outline" title={`first seen ${ts(o.first_seen)}`}>{o.type}: {o.value}{o.reputation ? ` · ${o.reputation.verdict ?? o.reputation.score}` : ""}</Badge>)}
          </div>
        ) : <span className="text-sm text-muted-foreground">No observables yet.</span>}
      </CardContent>
    </Card>
  );
}

function Tasks({ id }: { id: string }) {
  const qc = useQueryClient();
  const [title, setTitle] = useState("");
  const q = useQuery({ queryKey: ["tasks", id], queryFn: () => unwrap(api().GET("/cases/{id}/tasks", { params: { path: { id }, query: { limit: 200 } } })) });
  const add = useMutation({
    mutationFn: () => unwrap(api().POST("/cases/{id}/tasks", { params: { path: { id } }, body: { title } })),
    onSuccess: () => { setTitle(""); qc.invalidateQueries({ queryKey: ["tasks", id] }); },
  });
  const toggle = useMutation({
    mutationFn: (t: { id: string; done: boolean }) =>
      unwrap(api().PATCH("/cases/{id}/tasks/{task_id}", { params: { path: { id, task_id: t.id } }, body: { status: t.done ? "done" : "open" } })),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["tasks", id] }),
  });
  return (
    <Card>
      <CardContent className="flex flex-col gap-2 pt-4">
        <form className="flex gap-2" onSubmit={(e) => { e.preventDefault(); add.mutate(); }}>
          <Input value={title} onChange={(e) => setTitle(e.target.value)} placeholder="New task" />
          <Button type="submit" disabled={!title}><Plus className="h-4 w-4" /> Add</Button>
        </form>
        <ErrorBox error={add.error ?? toggle.error} />
        {(q.data?.items ?? []).map((t) => (
          <label key={t.id} className="flex items-center gap-2 rounded-md border p-2 text-sm">
            <input type="checkbox" checked={t.status === "done"} onChange={(e) => toggle.mutate({ id: t.id, done: e.target.checked })} />
            <span className={t.status === "done" ? "line-through text-muted-foreground" : ""}>{t.title}</span>
            <span className="ml-auto text-xs text-muted-foreground">{t.assignee ?? ""} {t.due_at ? `due ${ts(t.due_at)}` : ""}</span>
          </label>
        ))}
        {q.data && !q.data.items.length && <span className="text-sm text-muted-foreground">No tasks.</span>}
      </CardContent>
    </Card>
  );
}

function Evidence({ c }: { c: Case }) {
  const qc = useQueryClient();
  const file = useRef<HTMLInputElement>(null);
  const upload = useMutation({
    mutationFn: async (f: File) => raw(`/cases/${c.id}/evidence?filename=${encodeURIComponent(f.name)}`, { method: "POST", body: f, headers: { "Content-Type": f.type || "application/octet-stream" } }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["case", c.id] }),
  });
  const download = async (eid: string) => {
    const link = await unwrap(api().GET("/cases/{id}/evidence/{eid}", { params: { path: { id: c.id, eid } } }));
    window.open(link.url, "_blank", "noopener");
  };
  return (
    <Card>
      <CardContent className="flex flex-col gap-2 pt-4">
        <div>
          <input ref={file} type="file" className="hidden" onChange={(e) => e.target.files?.[0] && upload.mutate(e.target.files[0])} />
          <Button variant="outline" onClick={() => file.current?.click()} disabled={upload.isPending}><Paperclip className="h-4 w-4" /> Upload file (max 25 MB)</Button>
        </div>
        <ErrorBox error={upload.error} />
        {(c.evidence ?? []).map((e) => (
          <div key={e.id} className="flex items-center gap-2 rounded-md border p-2 text-sm">
            <span className="font-medium">{e.name}</span>
            <span className="font-mono text-xs text-muted-foreground" title={e.sha256}>{e.sha256.slice(0, 12)}… · {(e.size / 1024).toFixed(1)} KB</span>
            <span className="ml-auto text-xs text-muted-foreground">{e.added_by} · {ago(e.added_at)}</span>
            <Button size="sm" variant="ghost" onClick={() => download(e.id)}><Download className="h-4 w-4" /></Button>
          </div>
        ))}
      </CardContent>
    </Card>
  );
}

function Timeline({ id }: { id: string }) {
  const q = useQuery({ queryKey: ["timeline", id], queryFn: () => unwrap(api().GET("/cases/{id}/timeline", { params: { path: { id }, query: { limit: 200, sort: "-created_at" } } })) });
  if (q.isLoading) return <Loading />;
  return (
    <Card>
      <CardContent className="flex flex-col gap-2 pt-4">
        {(q.data?.items ?? []).map((ev) => (
          <div key={ev.id} className="flex gap-3 border-l-2 border-primary/40 pl-3 text-sm">
            <span className="w-36 shrink-0 text-xs text-muted-foreground">{ts(ev.created_at)}</span>
            <span className="font-medium">{ev.event_type}</span>
            <span className="text-muted-foreground">{ev.actor}</span>
            {ev.data && Object.keys(ev.data).length > 0 && <details className="ml-auto"><summary className="cursor-pointer text-xs">data</summary><JsonView value={ev.data} /></details>}
          </div>
        ))}
        {q.data && !q.data.items.length && <span className="text-sm text-muted-foreground">Nothing yet.</span>}
      </CardContent>
    </Card>
  );
}
