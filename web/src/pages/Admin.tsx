import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useSearchParams } from "react-router-dom";
import { toast } from "sonner";
import { AlertTriangle, Check, Copy, Gauge, KeyRound, Plus, RotateCcw, ScrollText, Search, Shield, Users } from "lucide-react";
import { api, unwrap, type Principal } from "@/lib/api";
import { bytes, num } from "@/lib/format";
import { requires, usePerms } from "@/lib/perms";
import { copy, cn } from "@/lib/utils";
import { Page } from "@/components/Layout";
import { Avatar, Empty, ErrorBox, PageHeader, SkeletonRows, Time } from "@/components/common";
import { Meter } from "@/components/charts";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card, Panel } from "@/components/ui/card";
import { Dialog, DialogContent, confirmDialog } from "@/components/ui/dialog";
import { FormField, Input, Select, Switch } from "@/components/ui/input";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";

const ROLES = ["admin", "engineer", "analyst", "viewer", "service"] as const;

export default function Admin() {
  const { can } = usePerms();
  const [params, setParams] = useSearchParams();
  const tabs = [
    can("principals:read") && { v: "principals", label: "Principals", icon: Users },
    can("principals:read") && { v: "roles", label: "Roles", icon: Shield },
    can("audit:read") && { v: "audit", label: "Audit log", icon: ScrollText },
    can("queue:read") && { v: "queues", label: "Queues & dead letters", icon: Gauge },
  ].filter(Boolean) as { v: string; label: string; icon: React.ElementType }[];
  const tab = tabs.some((t) => t.v === params.get("tab")) ? params.get("tab")! : tabs[0]?.v;
  return (
    <Page>
      <PageHeader title="Admin" subtitle="Who can do what, what they did, and how the queues are doing." />
      {!tabs.length ? <Card><Empty title="Nothing here for your roles" /></Card> : (
        <Tabs value={tab} onValueChange={(v) => setParams({ tab: v }, { replace: true })}>
          <TabsList>
            {tabs.map((t) => <TabsTrigger key={t.v} value={t.v}><t.icon className="size-3.5" /> {t.label}</TabsTrigger>)}
          </TabsList>
          <TabsContent value="principals"><Principals /></TabsContent>
          <TabsContent value="roles"><Roles /></TabsContent>
          <TabsContent value="audit"><Audit /></TabsContent>
          <TabsContent value="queues"><Queues /></TabsContent>
        </Tabs>
      )}
    </Page>
  );
}

function Principals() {
  const qc = useQueryClient();
  const { me, can } = usePerms();
  const q = useQuery({ queryKey: ["principals"], queryFn: () => unwrap(api().GET("/principals", { params: { query: { limit: 200 } } })) });
  const [open, setOpen] = useState(false);
  const patch = useMutation({
    mutationFn: (p: { id: string; body: { disabled?: boolean; roles?: string[] } }) => unwrap(api().PATCH("/principals/{id}", { params: { path: { id: p.id } }, body: p.body as never })),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["principals"] }),
    onError: (e) => toast.error("Update failed", { description: e instanceof Error ? e.message : String(e) }),
  });
  const del = useMutation({
    mutationFn: (id: string) => unwrap(api().DELETE("/principals/{id}", { params: { path: { id } } })),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["principals"] }); toast.success("Principal deleted"); },
  });
  const canWrite = can("principals:write");
  return (
    <Card className="overflow-hidden">
      <div className="flex items-center justify-between border-b border-line px-4 py-3">
        <div className="text-sm text-fg-3">API keys, service accounts and users with explicit roles. OIDC users get roles from the identity provider.</div>
        <Button variant="primary" size="sm" onClick={() => setOpen(true)} disabled={!canWrite} tip={canWrite ? undefined : requires("principals:write")}><Plus /> New principal</Button>
      </div>
      {q.isLoading ? <SkeletonRows rows={4} /> : q.error ? <div className="p-4"><ErrorBox error={q.error} /></div> : (
        <Table>
          <THead><TR><TH>Name</TH><TH>Type</TH><TH>Roles</TH><TH>Created</TH><TH>Active</TH><TH /></TR></THead>
          <TBody>
            {q.data!.items.map((p) => (
              <TR key={p.id}>
                <TD><span className="flex items-center gap-2"><Avatar name={p.name} size={22} /><span className="font-medium">{p.name}</span>{p.id === me?.id && <Badge tone="accent">you</Badge>}</span></TD>
                <TD><Badge tone="outline">{p.type === "api_key" ? <><KeyRound /> API key</> : p.type}</Badge></TD>
                <TD>
                  <Select
                    value={p.roles[0] ?? "viewer"}
                    disabled={!canWrite || p.id === me?.id}
                    onChange={(e) => patch.mutate({ id: p.id, body: { roles: [e.target.value] } })}
                    className="w-32"
                    aria-label={`Role of ${p.name}`}
                  >
                    {ROLES.map((r) => <option key={r}>{r}</option>)}
                  </Select>
                </TD>
                <TD className="text-sm text-fg-3"><Time iso={p.created_at} /></TD>
                <TD><Switch checked={!p.disabled} onChange={(on) => patch.mutate({ id: p.id, body: { disabled: !on } })} disabled={!canWrite || p.id === me?.id} label={`Enable ${p.name}`} /></TD>
                <TD className="text-right">
                  <Button
                    size="xs"
                    variant="danger-ghost"
                    disabled={!canWrite || p.id === me?.id}
                    onClick={async () => (await confirmDialog({ title: `Delete ${p.name}?`, body: p.type === "api_key" ? "The key stops working immediately." : undefined, confirm: "Delete", danger: true })) && del.mutate(p.id)}
                  >
                    Delete
                  </Button>
                </TD>
              </TR>
            ))}
          </TBody>
        </Table>
      )}
      <ErrorBox error={del.error} className="m-3" />
      {open && <NewPrincipal onClose={() => setOpen(false)} />}
    </Card>
  );
}

function NewPrincipal({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient();
  const [type, setType] = useState<Principal["type"]>("api_key");
  const [name, setName] = useState("");
  const [role, setRole] = useState("analyst");
  const [copied, setCopied] = useState(false);
  const m = useMutation({
    mutationFn: () => unwrap(api().POST("/principals", { body: { type, name, roles: [role as never] } })),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["principals"] }),
  });
  const key = m.data?.api_key;
  return (
    <Dialog open onOpenChange={(o) => !o && (!key || copied) && onClose()}>
      <DialogContent
        title={key ? "Copy the API key now" : "New principal"}
        description={key ? "It is shown only once and stored only as a hash." : undefined}
        footer={key ? <Button variant="primary" onClick={onClose} disabled={!copied} tip={copied ? undefined : "Copy the key first"}>Done</Button> : <><Button variant="ghost" onClick={onClose}>Cancel</Button><Button variant="primary" onClick={() => m.mutate()} disabled={!name.trim()} loading={m.isPending}>Create</Button></>}
      >
        {key ? (
          <div className="flex flex-col gap-3">
            <div className="flex gap-2">
              <Input readOnly value={key} className="flex-1 font-mono text-xs" onFocus={(e) => e.currentTarget.select()} />
              <Button onClick={async () => setCopied(await copy(key) || true)} variant={copied ? "secondary" : "primary"}>{copied ? <Check /> : <Copy />} {copied ? "Copied" : "Copy"}</Button>
            </div>
            <div className="flex gap-2 rounded-md border border-warn/30 bg-warn/10 px-3 py-2 text-xs text-warn"><AlertTriangle className="size-3.5 shrink-0" /> Store it in a secrets manager. If it is lost, delete this principal and create a new one.</div>
          </div>
        ) : (
          <>
            <FormField label="Type">
              <Select value={type} onChange={(e) => setType(e.target.value as Principal["type"])}>
                <option value="api_key">API key (scripts, integrations)</option>
                <option value="service">Service account</option>
                <option value="user">User (OIDC name, explicit roles)</option>
              </Select>
            </FormField>
            <FormField label="Name"><Input value={name} onChange={(e) => setName(e.target.value)} placeholder={type === "user" ? "dana.scully" : "wazuh-forwarder"} autoFocus /></FormField>
            <FormField label="Role"><Select value={role} onChange={(e) => setRole(e.target.value)}>{ROLES.map((r) => <option key={r}>{r}</option>)}</Select></FormField>
            <ErrorBox error={m.error} />
          </>
        )}
      </DialogContent>
    </Dialog>
  );
}

function Roles() {
  const q = useQuery({ queryKey: ["roles"], queryFn: () => unwrap(api().GET("/roles")) });
  const items = q.data?.items ?? [];
  const resources = [...new Set(items.flatMap((r) => r.permissions.map((p) => p.split(":")[0])))].sort();
  const has = (perms: string[], p: string) => perms.includes(p) || perms.includes(p.split(":")[0] + ":*") || perms.includes("*");
  return (
    <Card className="overflow-hidden">
      <ErrorBox error={q.error} className="m-3" />
      <Table>
        <THead>
          <TR><TH>Resource</TH>{items.map((r) => <TH key={r.name} className="text-center">{r.name}</TH>)}</TR>
        </THead>
        <TBody>
          {resources.map((res) => (
            <TR key={res}>
              <TD className="font-mono text-xs">{res}</TD>
              {items.map((r) => {
                const acts = ["read", "write", "run", "decide", "cancel"].filter((a) => has(r.permissions, `${res}:${a}`));
                return <TD key={r.name} className="text-center"><div className="flex flex-wrap justify-center gap-1">{acts.map((a) => <Badge key={a} tone={a === "read" ? "gray" : "accent"}>{a}</Badge>)}{!acts.length && <span className="text-fg-4">—</span>}</div></TD>;
              })}
            </TR>
          ))}
        </TBody>
      </Table>
    </Card>
  );
}

function Audit() {
  const [actor, setActor] = useState("");
  const q = useQuery({ queryKey: ["audit", actor], queryFn: () => unwrap(api().GET("/audit", { params: { query: { limit: 200, ...(actor ? { actor } : {}) } } })) });
  return (
    <Card className="overflow-hidden">
      <div className="flex items-center gap-3 border-b border-line px-4 py-3">
        <Input icon={<Search />} placeholder="Filter by actor (exact)" value={actor} onChange={(e) => setActor(e.target.value)} className="w-72" />
        <span className="text-xs text-fg-3">Every change and every secret read is recorded.</span>
      </div>
      {q.isLoading ? <SkeletonRows rows={8} /> : q.error ? <div className="p-4"><ErrorBox error={q.error} /></div> : !q.data!.items.length ? <Empty icon={<ScrollText />} title="No audit records" /> : (
        <Table>
          <THead><TR><TH>Time</TH><TH>Actor</TH><TH>Action</TH><TH>Resource</TH><TH>From</TH></TR></THead>
          <TBody>
            {q.data!.items.map((a) => (
              <TR key={a.id}>
                <TD className="whitespace-nowrap text-xs text-fg-2"><Time iso={a.created_at} mode="absolute" /></TD>
                <TD><span className="flex items-center gap-2"><Avatar name={a.actor} size={18} /><span className="text-sm">{a.actor}</span><span className="text-2xs text-fg-4">{a.actor_type}</span></span></TD>
                <TD><code className="rounded bg-hover px-1.5 py-0.5 text-xs">{a.action}</code></TD>
                <TD className="max-w-[320px] truncate font-mono text-xs text-fg-2" title={a.resource}>{a.resource}</TD>
                <TD className="font-mono text-xs text-fg-3">{a.ip ?? ""}</TD>
              </TR>
            ))}
          </TBody>
        </Table>
      )}
    </Card>
  );
}

function Queues() {
  const qc = useQueryClient();
  const { can } = usePerms();
  const stats = useQuery({ queryKey: ["queues"], queryFn: () => unwrap(api().GET("/queues/stats")), refetchInterval: 10_000 });
  const dlq = useQuery({ queryKey: ["dlq"], queryFn: () => unwrap(api().GET("/dlq", { params: { query: { limit: 50 } } })), refetchInterval: 30_000 });
  const replay = useMutation({
    mutationFn: (id: string) => unwrap(api().POST("/dlq/{id}/replay", { params: { path: { id } } })),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["dlq"] }); toast.success("Message replayed"); },
  });
  return (
    <div className="flex flex-col gap-4">
      <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
        {(stats.data?.streams ?? []).map((s) => {
          const fill = s.max_bytes > 0 ? s.bytes / s.max_bytes : 0;
          const pending = (s.consumers ?? []).reduce((a, c) => a + c.pending + c.ack_pending, 0);
          return (
            <Card key={s.name} className="flex flex-col gap-3 p-4">
              <div className="flex items-center justify-between">
                <span className="font-mono text-sm font-medium">{s.name}</span>
                <span className="tabular text-xs text-fg-3">{bytes(s.bytes)}{s.max_bytes > 0 ? ` / ${bytes(s.max_bytes)}` : ""}</span>
              </div>
              <div className="flex gap-6">
                <div><div className="text-xl font-semibold">{num(s.messages)}</div><div className="text-2xs text-fg-3">messages</div></div>
                <div><div className={cn("text-xl font-semibold", pending > 1000 && "text-warn")}>{num(pending)}</div><div className="text-2xs text-fg-3">pending</div></div>
              </div>
              {s.max_bytes > 0 && <Meter value={fill} tone={fill > 0.7 ? "crit" : fill > 0.4 ? "warn" : "accent"} />}
              {(s.consumers ?? []).length > 0 && (
                <div className="flex flex-col gap-1 border-t border-line pt-2">
                  {s.consumers!.map((c) => (
                    <div key={c.name} className="flex items-center justify-between text-2xs">
                      <span className="truncate font-mono text-fg-2">{c.name}</span>
                      <span className="tabular text-fg-3">{c.pending} pending · {c.ack_pending} in flight · {c.redelivered} redelivered</span>
                    </div>
                  ))}
                </div>
              )}
            </Card>
          );
        })}
      </div>
      <ErrorBox error={stats.error} />
      <Panel title="Dead letters" subtitle="Messages that failed every delivery. Fix the cause, then replay." icon={<AlertTriangle />} bodyClassName="p-0">
        {dlq.error ? <div className="p-4"><ErrorBox error={dlq.error} /></div> : !(dlq.data?.items.length) ? <Empty icon={<Check />} title="No dead letters" className="py-8" /> : (
          <Table>
            <THead><TR><TH>Seq</TH><TH>Origin</TH><TH>Reason</TH><TH className="text-right">Deliveries</TH><TH>Stored</TH><TH /></TR></THead>
            <TBody>
              {dlq.data!.items.map((d) => (
                <TR key={d.seq}>
                  <TD className="tabular text-xs">{d.seq}</TD>
                  <TD className="font-mono text-xs">{d.origin_subject}</TD>
                  <TD className="max-w-[420px] truncate text-xs text-fg-2" title={d.reason}>{d.reason}</TD>
                  <TD className="tabular text-right text-xs">{d.deliveries ?? ""}</TD>
                  <TD className="text-xs text-fg-3"><Time iso={d.stored_at} /></TD>
                  <TD className="text-right">
                    <Button size="xs" onClick={() => replay.mutate(String(d.seq))} loading={replay.isPending && replay.variables === String(d.seq)} disabled={!can("queue:write")} tip={can("queue:write") ? undefined : requires("queue:write")}><RotateCcw /> Replay</Button>
                  </TD>
                </TR>
              ))}
            </TBody>
          </Table>
        )}
      </Panel>
      <ErrorBox error={replay.error} />
    </div>
  );
}
