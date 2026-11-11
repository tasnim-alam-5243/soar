import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Copy, Plus } from "lucide-react";
import { api, unwrap } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input, Label, Select } from "@/components/ui/input";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Dialog, DialogContent } from "@/components/ui/dialog";
import { Badge } from "@/components/ui/badge";
import { ErrorBox, Loading, PageHeader } from "@/components/common";
import { ts } from "@/lib/utils";

export default function Admin() {
  return (
    <div>
      <PageHeader title="Admin" subtitle="Principals, roles, audit log and queues." />
      <Tabs defaultValue="principals">
        <TabsList>
          <TabsTrigger value="principals">Principals</TabsTrigger>
          <TabsTrigger value="roles">Roles</TabsTrigger>
          <TabsTrigger value="audit">Audit log</TabsTrigger>
          <TabsTrigger value="queues">Queues &amp; DLQ</TabsTrigger>
        </TabsList>
        <TabsContent value="principals"><Principals /></TabsContent>
        <TabsContent value="roles"><Roles /></TabsContent>
        <TabsContent value="audit"><Audit /></TabsContent>
        <TabsContent value="queues"><Queues /></TabsContent>
      </Tabs>
    </div>
  );
}

function Principals() {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["principals"], queryFn: () => unwrap(api().GET("/principals", { params: { query: { limit: 200 } } })) });
  const [open, setOpen] = useState(false);
  const toggle = useMutation({
    mutationFn: (p: { id: string; disabled: boolean }) => unwrap(api().PATCH("/principals/{id}", { params: { path: { id: p.id } }, body: { disabled: !p.disabled } })),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["principals"] }),
  });
  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between"><CardTitle>Principals</CardTitle><Button size="sm" onClick={() => setOpen(true)}><Plus className="h-4 w-4" /> New</Button></CardHeader>
      {q.isLoading ? <Loading /> : q.error ? <div className="p-3"><ErrorBox error={q.error} /></div> : (
        <Table>
          <THead><TR><TH>Name</TH><TH>Type</TH><TH>Roles</TH><TH>State</TH><TH></TH></TR></THead>
          <TBody>
            {q.data!.items.map((p) => (
              <TR key={p.id}>
                <TD className="font-medium">{p.name}</TD><TD>{p.type}</TD>
                <TD className="flex gap-1">{p.roles.map((r) => <Badge key={r}>{r}</Badge>)}</TD>
                <TD>{p.disabled ? <Badge tone="red">disabled</Badge> : <Badge tone="green">active</Badge>}</TD>
                <TD><Button size="sm" variant="ghost" onClick={() => toggle.mutate(p)}>{p.disabled ? "Enable" : "Disable"}</Button></TD>
              </TR>
            ))}
          </TBody>
        </Table>
      )}
      <ErrorBox error={toggle.error} />
      {open && <NewPrincipal onClose={() => setOpen(false)} />}
    </Card>
  );
}

function NewPrincipal({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient();
  const [type, setType] = useState<"api_key" | "service" | "user">("api_key");
  const [name, setName] = useState("");
  const [role, setRole] = useState("analyst");
  const m = useMutation({
    mutationFn: () => unwrap(api().POST("/principals", { body: { type, name, roles: [role as never] } })),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["principals"] }),
  });
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent title="New principal">
        {m.data?.api_key ? (
          <div className="flex flex-col gap-2 text-sm">
            <p>Copy the API key now; it is shown only once.</p>
            <div className="flex gap-2"><Input readOnly value={m.data.api_key} className="font-mono text-xs" /><Button variant="outline" onClick={() => navigator.clipboard.writeText(m.data!.api_key!)}><Copy className="h-4 w-4" /></Button></div>
            <Button onClick={onClose}>Done</Button>
          </div>
        ) : (
          <div className="flex flex-col gap-3">
            <div className="flex flex-col gap-1"><Label>Type</Label><Select value={type} onChange={(e) => setType(e.target.value as typeof type)}><option value="api_key">API key</option><option value="service">service</option><option value="user">user (OIDC name)</option></Select></div>
            <div className="flex flex-col gap-1"><Label>Name</Label><Input value={name} onChange={(e) => setName(e.target.value)} /></div>
            <div className="flex flex-col gap-1"><Label>Role</Label><Select value={role} onChange={(e) => setRole(e.target.value)}>{["admin", "engineer", "analyst", "viewer", "service"].map((r) => <option key={r}>{r}</option>)}</Select></div>
            <ErrorBox error={m.error} />
            <Button onClick={() => m.mutate()} disabled={!name || m.isPending}>Create</Button>
          </div>
        )}
      </DialogContent>
    </Dialog>
  );
}

function Roles() {
  const q = useQuery({ queryKey: ["roles"], queryFn: () => unwrap(api().GET("/roles")) });
  return (
    <div className="grid gap-3 md:grid-cols-2">
      {(q.data?.items ?? []).map((r) => (
        <Card key={r.name}><CardHeader><CardTitle>{r.name}</CardTitle></CardHeader>
          <CardContent className="flex flex-wrap gap-1">{r.permissions.map((p) => <Badge key={p} tone="outline">{p}</Badge>)}</CardContent></Card>
      ))}
      <ErrorBox error={q.error} />
    </div>
  );
}

function Audit() {
  const [actor, setActor] = useState("");
  const q = useQuery({ queryKey: ["audit", actor], queryFn: () => unwrap(api().GET("/audit", { params: { query: { limit: 200, actor: actor || undefined } } })) });
  return (
    <Card>
      <CardContent className="pt-4"><Input placeholder="Filter by actor" value={actor} onChange={(e) => setActor(e.target.value)} className="mb-2 w-64" /></CardContent>
      {q.isLoading ? <Loading /> : q.error ? <div className="p-3"><ErrorBox error={q.error} /></div> : (
        <Table>
          <THead><TR><TH>Time</TH><TH>Actor</TH><TH>Action</TH><TH>Resource</TH><TH>IP</TH></TR></THead>
          <TBody>{q.data!.items.map((a) => (
            <TR key={a.id}><TD className="text-xs">{ts(a.created_at)}</TD><TD>{a.actor} <span className="text-xs text-muted-foreground">({a.actor_type})</span></TD>
              <TD className="font-mono text-xs">{a.action}</TD><TD className="font-mono text-xs">{a.resource}</TD><TD className="text-xs">{a.ip}</TD></TR>
          ))}</TBody>
        </Table>
      )}
    </Card>
  );
}

function Queues() {
  const qc = useQueryClient();
  const stats = useQuery({ queryKey: ["queues"], queryFn: () => unwrap(api().GET("/queues/stats")), refetchInterval: 10_000 });
  const dlq = useQuery({ queryKey: ["dlq"], queryFn: () => unwrap(api().GET("/dlq", { params: { query: { limit: 50 } } })) });
  const replay = useMutation({ mutationFn: (id: string) => unwrap(api().POST("/dlq/{id}/replay", { params: { path: { id } } })), onSuccess: () => qc.invalidateQueries({ queryKey: ["dlq"] }) });
  return (
    <div className="grid gap-3">
      <Card><CardHeader><CardTitle>Streams</CardTitle></CardHeader>
        <Table><THead><TR><TH>Stream</TH><TH>Messages</TH><TH>Bytes</TH><TH>Consumers (pending / ack pending / redelivered)</TH></TR></THead>
          <TBody>{(stats.data?.streams ?? []).map((s) => (
            <TR key={s.name}><TD className="font-mono">{s.name}</TD><TD>{s.messages}</TD><TD>{s.bytes}{s.max_bytes > 0 ? ` / ${s.max_bytes}` : ""}</TD>
              <TD className="text-xs">{(s.consumers ?? []).map((c) => `${c.name}: ${c.pending}/${c.ack_pending}/${c.redelivered}`).join(" · ")}</TD></TR>
          ))}</TBody></Table>
        <ErrorBox error={stats.error} />
      </Card>
      <Card><CardHeader><CardTitle>Dead letters</CardTitle></CardHeader>
        <Table><THead><TR><TH>Seq</TH><TH>Origin</TH><TH>Reason</TH><TH>Deliveries</TH><TH>Stored</TH><TH></TH></TR></THead>
          <TBody>{(dlq.data?.items ?? []).map((d) => (
            <TR key={d.seq}><TD>{d.seq}</TD><TD className="font-mono text-xs">{d.origin_subject}</TD><TD className="text-xs">{d.reason}</TD><TD>{d.deliveries ?? ""}</TD><TD className="text-xs">{ts(d.stored_at)}</TD>
              <TD><Button size="sm" variant="outline" onClick={() => replay.mutate(String(d.seq))}>Replay</Button></TD></TR>
          ))}</TBody></Table>
        <ErrorBox error={dlq.error ?? replay.error} />
      </Card>
    </div>
  );
}
