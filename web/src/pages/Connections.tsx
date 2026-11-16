import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Blocks, Cable, KeyRound, Lock, Plug, Plus, Trash2 } from "lucide-react";
import { api, unwrap } from "@/lib/api";
import { requires, usePerms } from "@/lib/perms";
import { Page } from "@/components/Layout";
import { Empty, ErrorBox, JsonView, PageHeader, SkeletonRows, StatusBadge, Time } from "@/components/common";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card, Panel } from "@/components/ui/card";
import { Dialog, DialogContent, confirmDialog } from "@/components/ui/dialog";
import { FormField, Input, Select, Textarea } from "@/components/ui/input";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";

type TestResult = { ok: boolean; status: string; execution_id: string; output?: unknown; error?: string };

export default function Connections() {
  const qc = useQueryClient();
  const { can } = usePerms();
  const conns = useQuery({ queryKey: ["connections"], queryFn: () => unwrap(api().GET("/connections", { params: { query: { limit: 200 } } })) });
  const connectors = useQuery({ queryKey: ["connectors"], queryFn: () => unwrap(api().GET("/connectors")), enabled: can("connectors:read") });
  const secrets = useQuery({ queryKey: ["secrets"], queryFn: () => unwrap(api().GET("/secrets", { params: { query: { limit: 200 } } })), enabled: can("secrets:read_metadata") });
  const [adding, setAdding] = useState(false);
  const [addingSecret, setAddingSecret] = useState(false);
  const [results, setResults] = useState<Record<string, TestResult>>({});
  const [shown, setShown] = useState<string | null>(null);
  const del = useMutation({ mutationFn: (id: string) => unwrap(api().DELETE("/connections/{id}", { params: { path: { id } } })), onSuccess: () => { qc.invalidateQueries({ queryKey: ["connections"] }); toast.success("Connection deleted"); } });
  const test = useMutation({
    mutationFn: (id: string) => unwrap(api().POST("/connections/{id}/test", { params: { path: { id } } })),
    onSuccess: (r, id) => { setResults((x) => ({ ...x, [id]: r as TestResult })); (r as TestResult).ok ? toast.success("Connection works") : toast.error("Connection test failed", { description: (r as TestResult).error }); },
  });
  const delSecret = useMutation({ mutationFn: (name: string) => unwrap(api().DELETE("/secrets/{name}", { params: { path: { name } } })), onSuccess: () => qc.invalidateQueries({ queryKey: ["secrets"] }) });
  const canWrite = can("connections:write");
  return (
    <Page>
      <PageHeader
        title="Connections"
        subtitle="Credentials and settings integrations use. Secret values are write-only and decrypted only inside the worker."
        actions={
          <>
            <Button onClick={() => setAddingSecret(true)} disabled={!can("secrets:write")} tip={can("secrets:write") ? undefined : requires("secrets:write")}><KeyRound /> New secret</Button>
            <Button variant="primary" onClick={() => setAdding(true)} disabled={!canWrite} tip={canWrite ? undefined : requires("connections:write")}><Plus /> New connection</Button>
          </>
        }
      />
      <Card className="overflow-hidden">
        {conns.isLoading ? <SkeletonRows rows={3} /> : conns.error ? <div className="p-4"><ErrorBox error={conns.error} /></div> : !conns.data!.items.length ? (
          <Empty icon={<Cable />} title="No connections" text="Connect DFIR-IRIS, Microsoft Graph and other products so workflows can act on them." />
        ) : (
          <Table>
            <THead><TR><TH>Connection</TH><TH>Connector</TH><TH>Auth</TH><TH>Secrets</TH><TH>Last test</TH><TH>Created</TH><TH /></TR></THead>
            <TBody>
              {conns.data!.items.map((c) => {
                const r = results[c.id];
                return (
                  <TR key={c.id}>
                    <TD className="font-medium">{c.name}</TD>
                    <TD><Badge>{c.connector_id}</Badge></TD>
                    <TD className="text-sm text-fg-2">{c.auth_type}</TD>
                    <TD>
                      <div className="flex flex-wrap gap-1">
                        {Object.entries(c.secret_refs ?? {}).map(([k, v]) => <Badge key={k} tone="outline" className="font-mono"><Lock />{k} → {String(v)}</Badge>)}
                        {!Object.keys(c.secret_refs ?? {}).length && <span className="text-fg-4">—</span>}
                      </div>
                    </TD>
                    <TD>{r ? <button onClick={() => setShown(c.id)} className="hover:underline"><StatusBadge status={r.ok ? "succeeded" : "failed"} /></button> : <span className="text-xs text-fg-4">not tested</span>}</TD>
                    <TD className="text-sm text-fg-3"><Time iso={c.created_at} /></TD>
                    <TD>
                      <div className="flex justify-end gap-1">
                        <Button size="sm" variant="ghost" onClick={() => test.mutate(c.id)} loading={test.isPending && test.variables === c.id} disabled={!can("connections:read")}><Plug /> Test</Button>
                        <Button
                          size="icon-sm"
                          variant="danger-ghost"
                          disabled={!canWrite}
                          tip={canWrite ? "Delete" : requires("connections:write")}
                          aria-label={`Delete ${c.name}`}
                          onClick={async () => (await confirmDialog({ title: `Delete ${c.name}?`, body: "Workflows that use this connection will fail until you create it again.", confirm: "Delete", danger: true })) && del.mutate(c.id)}
                        >
                          <Trash2 />
                        </Button>
                      </div>
                    </TD>
                  </TR>
                );
              })}
            </TBody>
          </Table>
        )}
      </Card>
      <ErrorBox error={del.error ?? test.error} className="mt-3" />

      <div className="mt-6 grid gap-4 lg:grid-cols-2">
        <Panel title="Secrets" subtitle="Envelope-encrypted; values can never be read back" icon={<KeyRound />} bodyClassName="p-0">
          {secrets.error ? <div className="p-4"><ErrorBox error={secrets.error} /></div> : !(secrets.data?.items.length) ? <div className="px-4 py-6 text-center text-sm text-fg-4">{can("secrets:read_metadata") ? "No secrets stored" : requires("secrets:read_metadata")}</div> : (
            <ul className="divide-y divide-line">
              {secrets.data!.items.map((s) => (
                <li key={s.id} className="flex items-center gap-3 px-4 py-2.5">
                  <Lock className="size-3.5 text-fg-3" />
                  <code className="text-sm">{s.name}</code>
                  <span className="text-xs text-fg-4">key {s.key_id} · <Time iso={s.updated_at} /></span>
                  <Button
                    className="ml-auto"
                    size="icon-xs"
                    variant="danger-ghost"
                    disabled={!can("secrets:write")}
                    aria-label={`Delete secret ${s.name}`}
                    onClick={async () => (await confirmDialog({ title: `Delete secret ${s.name}?`, body: "Connections that reference it stop working.", confirm: "Delete", danger: true })) && delSecret.mutate(s.name)}
                  >
                    <Trash2 />
                  </Button>
                </li>
              ))}
            </ul>
          )}
        </Panel>
        <Panel title="Available connectors" icon={<Blocks />} bodyClassName="p-0">
          <ul className="divide-y divide-line">
            {(connectors.data?.items ?? []).map((c) => (
              <li key={c.id}>
                <details className="group px-4 py-2.5">
                  <summary className="flex cursor-pointer list-none items-center gap-2">
                    <span className="font-medium">{c.title ?? c.id}</span>
                    {c.builtin && <Badge>built-in</Badge>}
                    <span className="ml-auto text-xs text-fg-3">{c.actions.length} actions</span>
                  </summary>
                  <div className="mt-2 flex flex-wrap gap-1">{c.actions.map((a) => <Badge key={a} tone="outline" className="font-mono">{a}</Badge>)}</div>
                </details>
              </li>
            ))}
          </ul>
        </Panel>
      </div>
      {adding && <NewConnection connectors={(connectors.data?.items ?? []).filter((c) => !c.builtin)} onClose={() => setAdding(false)} />}
      {addingSecret && <NewSecret onClose={() => setAddingSecret(false)} />}
      {shown && results[shown] && (
        <Dialog open onOpenChange={(o) => !o && setShown(null)}>
          <DialogContent wide title="Connection test result">
            <JsonView value={results[shown]} />
          </DialogContent>
        </Dialog>
      )}
    </Page>
  );
}

function NewConnection({ connectors, onClose }: { connectors: { id: string; title?: string; secrets?: string[]; connection_schema?: Record<string, unknown>; auth_type?: string }[]; onClose: () => void }) {
  const qc = useQueryClient();
  const [connector, setConnector] = useState(connectors[0]?.id ?? "");
  const sel = connectors.find((c) => c.id === connector);
  const [name, setName] = useState("");
  const [auth, setAuth] = useState(sel?.auth_type ?? "bearer");
  const [config, setConfig] = useState('{\n  "base_url": "https://"\n}');
  const [refs, setRefs] = useState(JSON.stringify(Object.fromEntries((sel?.secrets ?? ["api_key"]).map((s) => [s, "name_of_a_stored_secret"])), null, 2));
  const parse = (s: string) => { try { JSON.parse(s); return null; } catch (e) { return e instanceof Error ? e.message : String(e); } };
  const m = useMutation({
    mutationFn: () => unwrap(api().POST("/connections", { body: { connector_id: connector, name, auth_type: auth as never, config: JSON.parse(config), secret_refs: JSON.parse(refs) } })),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["connections"] }); toast.success("Connection created"); onClose(); },
  });
  const bad = parse(config) ?? parse(refs);
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent
        wide
        title="New connection"
        footer={<><Button variant="ghost" onClick={onClose}>Cancel</Button><Button variant="primary" onClick={() => m.mutate()} disabled={!name || !connector || !!bad} loading={m.isPending}>Create connection</Button></>}
      >
        <div className="grid gap-4 sm:grid-cols-3">
          <FormField label="Connector">
            <Select value={connector} onChange={(e) => { setConnector(e.target.value); const c = connectors.find((x) => x.id === e.target.value); if (c?.auth_type) setAuth(c.auth_type); setRefs(JSON.stringify(Object.fromEntries((c?.secrets ?? ["api_key"]).map((s) => [s, "name_of_a_stored_secret"])), null, 2)); }}>
              {connectors.map((c) => <option key={c.id} value={c.id}>{c.title ?? c.id}</option>)}
            </Select>
          </FormField>
          <FormField label="Name"><Input value={name} onChange={(e) => setName(e.target.value)} placeholder="iris-prod" className="font-mono" /></FormField>
          <FormField label="Auth">
            <Select value={auth} onChange={(e) => setAuth(e.target.value)}>{["none", "api_key", "basic", "bearer", "oauth2_client_credentials"].map((a) => <option key={a}>{a}</option>)}</Select>
          </FormField>
        </div>
        <div className="grid gap-4 sm:grid-cols-2">
          <FormField label="Config (JSON)" error={parse(config) ?? undefined}><Textarea value={config} onChange={(e) => setConfig(e.target.value)} className="min-h-[140px]" spellCheck={false} /></FormField>
          <FormField label="Secret references" hint="Credential name → name of a stored secret" error={parse(refs) ?? undefined}><Textarea value={refs} onChange={(e) => setRefs(e.target.value)} className="min-h-[140px]" spellCheck={false} /></FormField>
        </div>
        {sel?.connection_schema && (
          <details>
            <summary className="cursor-pointer text-xs text-fg-3 hover:text-fg">Config schema for {sel.title ?? sel.id}</summary>
            <JsonView value={sel.connection_schema} className="mt-2" />
          </details>
        )}
        <ErrorBox error={m.error} />
      </DialogContent>
    </Dialog>
  );
}

function NewSecret({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [value, setValue] = useState("");
  const m = useMutation({
    mutationFn: () => unwrap(api().PUT("/secrets/{name}", { params: { path: { name } }, body: { value } })),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["secrets"] }); toast.success(`Secret ${name} stored`); onClose(); },
  });
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent
        title="New secret"
        description="Encrypted with the platform master key. It can never be read back, only replaced."
        footer={<><Button variant="ghost" onClick={onClose}>Cancel</Button><Button variant="primary" onClick={() => m.mutate()} disabled={!name || !value} loading={m.isPending}><Lock /> Store secret</Button></>}
      >
        <FormField label="Name"><Input value={name} onChange={(e) => setName(e.target.value)} placeholder="iris_api_key" className="font-mono" autoFocus /></FormField>
        <FormField label="Value"><Input type="password" value={value} onChange={(e) => setValue(e.target.value)} autoComplete="new-password" /></FormField>
        <ErrorBox error={m.error} />
      </DialogContent>
    </Dialog>
  );
}
