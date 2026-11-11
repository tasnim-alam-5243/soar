import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { FlaskConical } from "lucide-react";
import { api, unwrap, type Source } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Textarea } from "@/components/ui/input";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { Dialog, DialogContent } from "@/components/ui/dialog";
import { Empty, ErrorBox, JsonView, Loading, PageHeader, StatusBadge } from "@/components/common";
import { ago } from "@/lib/utils";

export default function Sources() {
  const q = useQuery({ queryKey: ["sources"], queryFn: () => unwrap(api().GET("/sources", { params: { query: { limit: 200, sort: "name" } } })) });
  const [testing, setTesting] = useState<Source | null>(null);
  const qc = useQueryClient();
  const toggle = useMutation({
    mutationFn: (s: Source) => unwrap(api().PATCH("/sources/{id}", { params: { path: { id: s.id } }, body: { enabled: !s.enabled } as never })),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["sources"] }),
  });
  return (
    <div>
      <PageHeader title="Sources" subtitle="Where events come from: adapter (transport) + parser + mapping + filter sets." />
      <Card>
        {q.isLoading ? <Loading /> : q.error ? <div className="p-3"><ErrorBox error={q.error} /></div> : q.data!.items.length === 0 ? <Empty text="No sources" /> : (
          <Table>
            <THead><TR><TH>Name</TH><TH>Adapter</TH><TH>Mapping</TH><TH>Filters</TH><TH>Health</TH><TH>Last event</TH><TH></TH></TR></THead>
            <TBody>
              {q.data!.items.map((s) => (
                <TR key={s.id}>
                  <TD className="font-medium">{s.name}<div className="text-xs font-normal text-muted-foreground">{s.system}</div></TD>
                  <TD>{s.adapter} / {s.parser}</TD>
                  <TD className="font-mono text-xs">{s.mapping_id}</TD>
                  <TD className="font-mono text-xs">{(s.filters ?? []).join(", ") || "—"}</TD>
                  <TD>{s.enabled ? <StatusBadge status={s.health?.status ?? "unknown"} /> : <StatusBadge status="disabled" />}</TD>
                  <TD className="text-muted-foreground">{ago(s.health?.last_event_at)}</TD>
                  <TD className="flex gap-1">
                    <Button size="sm" variant="outline" onClick={() => setTesting(s)}><FlaskConical className="h-3.5 w-3.5" /> Test</Button>
                    <Button size="sm" variant="ghost" onClick={() => toggle.mutate(s)}>{s.enabled ? "Disable" : "Enable"}</Button>
                  </TD>
                </TR>
              ))}
            </TBody>
          </Table>
        )}
      </Card>
      <ErrorBox error={toggle.error} />
      {testing && <TestSource s={testing} onClose={() => setTesting(null)} />}
    </div>
  );
}

function TestSource({ s, onClose }: { s: Source; onClose: () => void }) {
  const [payload, setPayload] = useState("{\n  \"id\": \"1793256511.10907\",\n  \"timestamp\": \"2026-10-29T12:48:31.597+0600\",\n  \"rule\": { \"id\": \"5715\", \"level\": 3, \"description\": \"sshd: authentication success.\" },\n  \"agent\": { \"id\": \"001\", \"name\": \"web-1\" },\n  \"data\": { \"srcip\": \"10.10.192.1\", \"dstuser\": \"root\" }\n}");
  const m = useMutation({ mutationFn: () => unwrap(api().POST("/sources/{id}/test", { params: { path: { id: s.id } }, body: { payload: JSON.parse(payload) } })) });
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent title={`Test ${s.name}`} className="max-w-3xl">
        <p className="text-sm text-muted-foreground">Runs a sample through the parser, filters and mapping. Nothing is published.</p>
        <Textarea className="h-48" value={payload} onChange={(e) => setPayload(e.target.value)} />
        <Button onClick={() => m.mutate()} disabled={m.isPending}>Run test</Button>
        <ErrorBox error={m.error} />
        {m.data && <CardContent className="p-0"><JsonView value={m.data} /></CardContent>}
      </DialogContent>
    </Dialog>
  );
}
