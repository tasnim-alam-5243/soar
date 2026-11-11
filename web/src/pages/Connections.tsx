import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { KeyRound, Plug, Plus, Trash2 } from "lucide-react";
import { api, unwrap } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input, Label, Select, Textarea } from "@/components/ui/input";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { Dialog, DialogContent } from "@/components/ui/dialog";
import { Badge } from "@/components/ui/badge";
import { Empty, ErrorBox, JsonView, Loading, PageHeader } from "@/components/common";
import { ago } from "@/lib/utils";

export default function Connections() {
  const qc = useQueryClient();
  const conns = useQuery({ queryKey: ["connections"], queryFn: () => unwrap(api().GET("/connections", { params: { query: { limit: 200 } } })) });
  const connectors = useQuery({ queryKey: ["connectors"], queryFn: () => unwrap(api().GET("/connectors")) });
  const secrets = useQuery({ queryKey: ["secrets"], queryFn: () => unwrap(api().GET("/secrets", { params: { query: { limit: 200 } } })) });
  const [adding, setAdding] = useState(false);
  const [addingSecret, setAddingSecret] = useState(false);
  const [testResult, setTestResult] = useState<unknown>(null);
  const del = useMutation({ mutationFn: (id: string) => unwrap(api().DELETE("/connections/{id}", { params: { path: { id } } })), onSuccess: () => qc.invalidateQueries({ queryKey: ["connections"] }) });
  const test = useMutation({ mutationFn: (id: string) => unwrap(api().POST("/connections/{id}/test", { params: { path: { id } } })), onSuccess: setTestResult });
  const delSecret = useMutation({ mutationFn: (name: string) => unwrap(api().DELETE("/secrets/{name}", { params: { path: { name } } })), onSuccess: () => qc.invalidateQueries({ queryKey: ["secrets"] }) });
  return (
    <div>
      <PageHeader title="Connections" subtitle="Credentials and settings integrations use. Secret values are write-only and decrypted only inside the worker."
        actions={<><Button variant="outline" onClick={() => setAddingSecret(true)}><KeyRound className="h-4 w-4" /> New secret</Button><Button onClick={() => setAdding(true)}><Plus className="h-4 w-4" /> New connection</Button></>} />
      <Card>
        {conns.isLoading ? <Loading /> : conns.error ? <div className="p-3"><ErrorBox error={conns.error} /></div> : conns.data!.items.length === 0 ? <Empty text="No connections" /> : (
          <Table>
            <THead><TR><TH>Name</TH><TH>Connector</TH><TH>Auth</TH><TH>Secrets</TH><TH>Created</TH><TH></TH></TR></THead>
            <TBody>
              {conns.data!.items.map((c) => (
                <TR key={c.id}>
                  <TD className="font-medium">{c.name}</TD>
                  <TD>{c.connector_id}</TD>
                  <TD>{c.auth_type}</TD>
                  <TD className="font-mono text-xs">{Object.entries(c.secret_refs ?? {}).map(([k, v]) => `${k} → ${v}`).join(", ") || "—"}</TD>
                  <TD className="text-muted-foreground">{ago(c.created_at)}</TD>
                  <TD className="flex gap-1">
                    <Button size="sm" variant="outline" onClick={() => test.mutate(c.id)} disabled={test.isPending}><Plug className="h-3.5 w-3.5" /> Test</Button>
                    <Button size="sm" variant="ghost" onClick={() => confirm(`Delete ${c.name}?`) && del.mutate(c.id)}><Trash2 className="h-3.5 w-3.5" /></Button>
                  </TD>
                </TR>
              ))}
            </TBody>
          </Table>
        )}
      </Card>
      <ErrorBox error={del.error ?? test.error} />
      {testResult !== null && <Card className="mt-3"><CardHeader><CardTitle>Connection test</CardTitle></CardHeader><CardContent><JsonView value={testResult} /></CardContent></Card>}
      <div className="mt-4 grid gap-3 lg:grid-cols-2">
        <Card>
          <CardHeader><CardTitle>Secrets</CardTitle></CardHeader>
          <CardContent className="flex flex-col gap-1.5 text-sm">
            {(secrets.data?.items ?? []).map((s) => (
              <div key={s.id} className="flex items-center gap-2">
                <code className="text-xs">{s.name}</code><span className="text-xs text-muted-foreground">key {s.key_id} · {ago(s.updated_at)}</span>
                <Button className="ml-auto" size="sm" variant="ghost" onClick={() => confirm(`Delete secret ${s.name}?`) && delSecret.mutate(s.name)}><Trash2 className="h-3.5 w-3.5" /></Button>
              </div>
            ))}
            {secrets.error && <ErrorBox error={secrets.error} />}
          </CardContent>
        </Card>
        <Card>
          <CardHeader><CardTitle>Available connectors</CardTitle></CardHeader>
          <CardContent className="flex flex-col gap-2 text-sm">
            {(connectors.data?.items ?? []).map((c) => (
              <details key={c.id}>
                <summary className="cursor-pointer"><span className="font-medium">{c.title ?? c.id}</span> <span className="text-xs text-muted-foreground">({c.actions.length} actions)</span></summary>
                <div className="mt-1 flex flex-wrap gap-1">{c.actions.map((a) => <Badge key={a} tone="outline">{a}</Badge>)}</div>
              </details>
            ))}
          </CardContent>
        </Card>
      </div>
      {adding && <NewConnection connectors={(connectors.data?.items ?? []).filter((c) => !c.builtin)} onClose={() => setAdding(false)} />}
      {addingSecret && <NewSecret onClose={() => setAddingSecret(false)} />}
    </div>
  );
}

function NewConnection({ connectors, onClose }: { connectors: { id: string; title?: string; secrets?: string[]; connection_schema?: Record<string, unknown> }[]; onClose: () => void }) {
  const qc = useQueryClient();
  const [connector, setConnector] = useState(connectors[0]?.id ?? "");
  const [name, setName] = useState("");
  const [auth, setAuth] = useState("bearer");
  const [config, setConfig] = useState("{\n  \"base_url\": \"https://\"\n}");
  const [refs, setRefs] = useState("{\n  \"api_key\": \"name_of_a_stored_secret\"\n}");
  const m = useMutation({
    mutationFn: () => unwrap(api().POST("/connections", { body: { connector_id: connector, name, auth_type: auth as never, config: JSON.parse(config), secret_refs: JSON.parse(refs) } })),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["connections"] }); onClose(); },
  });
  const sel = connectors.find((c) => c.id === connector);
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent title="New connection" className="max-w-2xl">
        <div className="grid grid-cols-2 gap-3">
          <div className="flex flex-col gap-1"><Label>Connector</Label><Select value={connector} onChange={(e) => setConnector(e.target.value)}>{connectors.map((c) => <option key={c.id} value={c.id}>{c.title ?? c.id}</option>)}</Select></div>
          <div className="flex flex-col gap-1"><Label>Name</Label><Input value={name} onChange={(e) => setName(e.target.value)} placeholder="iris-prod" /></div>
          <div className="flex flex-col gap-1"><Label>Auth</Label><Select value={auth} onChange={(e) => setAuth(e.target.value)}>{["none", "api_key", "basic", "bearer", "oauth2_client_credentials"].map((a) => <option key={a}>{a}</option>)}</Select></div>
          <div className="text-xs text-muted-foreground">Secrets this connector expects: {(sel?.secrets ?? []).join(", ") || "—"}</div>
        </div>
        <div><Label>Config (JSON)</Label><Textarea value={config} onChange={(e) => setConfig(e.target.value)} /></div>
        {sel?.connection_schema && <details><summary className="cursor-pointer text-xs text-muted-foreground">Config schema</summary><JsonView value={sel.connection_schema} /></details>}
        <div><Label>Secret references (credential name → stored secret name)</Label><Textarea value={refs} onChange={(e) => setRefs(e.target.value)} /></div>
        <ErrorBox error={m.error} />
        <Button onClick={() => m.mutate()} disabled={!name || !connector || m.isPending}>Create</Button>
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
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["secrets"] }); onClose(); },
  });
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent title="New secret">
        <div className="flex flex-col gap-1"><Label>Name</Label><Input value={name} onChange={(e) => setName(e.target.value)} placeholder="iris_api_key" /></div>
        <div className="flex flex-col gap-1"><Label>Value</Label><Input type="password" value={value} onChange={(e) => setValue(e.target.value)} autoComplete="off" /></div>
        <p className="text-xs text-muted-foreground">The value is encrypted with the platform master key and can never be read back.</p>
        <ErrorBox error={m.error} />
        <Button onClick={() => m.mutate()} disabled={!name || !value || m.isPending}>Store</Button>
      </DialogContent>
    </Dialog>
  );
}
