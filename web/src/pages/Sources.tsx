import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { ArrowRight, FlaskConical, Radio } from "lucide-react";
import { api, unwrap, type Source } from "@/lib/api";
import { pct } from "@/lib/format";
import { requires, usePerms } from "@/lib/perms";
import { Page } from "@/components/Layout";
import { Empty, ErrorBox, JsonView, PageHeader, SkeletonRows, StatusBadge, Time } from "@/components/common";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Dialog, DialogContent } from "@/components/ui/dialog";
import { Switch, Textarea } from "@/components/ui/input";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";

export default function Sources() {
  const { can } = usePerms();
  const q = useQuery({ queryKey: ["sources"], queryFn: () => unwrap(api().GET("/sources", { params: { query: { limit: 200, sort: "name" } } })), refetchInterval: 30_000 });
  const [testing, setTesting] = useState<Source | null>(null);
  const qc = useQueryClient();
  const toggle = useMutation({
    mutationFn: (s: Source) => unwrap(api().PATCH("/sources/{id}", { params: { path: { id: s.id } }, body: { enabled: !s.enabled } as never })),
    onSuccess: (s) => { qc.invalidateQueries({ queryKey: ["sources"] }); toast.success(`${s.name} ${s.enabled ? "enabled" : "disabled"}`); },
  });
  const canWrite = can("sources:write");
  return (
    <Page>
      <PageHeader title="Sources" subtitle="Where events come from: transport adapter, parser, mapping and filter sets." />
      <Card className="overflow-hidden">
        {q.isLoading ? <SkeletonRows rows={4} /> : q.error ? <div className="p-4"><ErrorBox error={q.error} /></div> : !q.data!.items.length ? (
          <Empty icon={<Radio />} title="No sources" text="Run `soar seed` to install the built-in Wazuh source pack." />
        ) : (
          <Table>
            <THead><TR><TH>Source</TH><TH>Pipeline</TH><TH>Health</TH><TH className="text-right">Rate</TH><TH className="text-right">Errors</TH><TH>Last event</TH><TH>Enabled</TH><TH /></TR></THead>
            <TBody>
              {q.data!.items.map((s) => (
                <TR key={s.id}>
                  <TD>
                    <div className="font-medium">{s.name}</div>
                    <div className="text-xs text-fg-3">{s.system}{s.schedule ? ` · every ${s.schedule}` : ""}</div>
                  </TD>
                  <TD>
                    <div className="flex flex-wrap items-center gap-1 text-xs">
                      <Badge>{s.adapter}</Badge><ArrowRight className="size-3 text-fg-4" /><Badge>{s.parser}</Badge><ArrowRight className="size-3 text-fg-4" />
                      <Badge tone="outline" className="font-mono">{s.mapping_id}</Badge>
                      {(s.filters ?? []).length > 0 && <span className="text-fg-3">+{s.filters!.length} filter{s.filters!.length > 1 ? "s" : ""}</span>}
                    </div>
                  </TD>
                  <TD><StatusBadge status={s.enabled ? s.health?.status || "unknown" : "disabled"} /></TD>
                  <TD className="tabular text-right text-sm">{s.health?.rate_per_sec ? `${s.health.rate_per_sec.toFixed(1)}/s` : "—"}</TD>
                  <TD className="tabular text-right text-sm">{s.health?.error_rate ? <span className={s.health.error_rate > 0.05 ? "text-crit" : ""}>{pct(s.health.error_rate)}</span> : "—"}</TD>
                  <TD className="text-sm text-fg-2"><Time iso={s.health?.last_event_at} /></TD>
                  <TD><Switch checked={s.enabled} onChange={() => toggle.mutate(s)} disabled={!canWrite || toggle.isPending} label={`Enable ${s.name}`} /></TD>
                  <TD className="text-right"><Button size="sm" variant="ghost" onClick={() => setTesting(s)} disabled={!can("sources:read")} tip={can("sources:read") ? undefined : requires("sources:read")}><FlaskConical /> Test</Button></TD>
                </TR>
              ))}
            </TBody>
          </Table>
        )}
      </Card>
      <ErrorBox error={toggle.error} className="mt-3" />
      {testing && <TestSource s={testing} onClose={() => setTesting(null)} />}
    </Page>
  );
}

const sample = `{
  "id": "1793256511.10907",
  "timestamp": "2026-10-29T12:48:31.597+0600",
  "rule": { "id": "5715", "level": 3, "description": "sshd: authentication success." },
  "agent": { "id": "001", "name": "web-1" },
  "data": { "srcip": "10.10.192.1", "dstuser": "root" }
}`;

function TestSource({ s, onClose }: { s: Source; onClose: () => void }) {
  const [payload, setPayload] = useState(sample);
  let parseError: string | null = null;
  try { JSON.parse(payload); } catch (e) { parseError = e instanceof Error ? e.message : String(e); }
  const m = useMutation({ mutationFn: () => unwrap(api().POST("/sources/{id}/test", { params: { path: { id: s.id } }, body: { payload: JSON.parse(payload) } })) });
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent wide title={`Test ${s.name}`} description="Runs a sample through the parser, filters and mapping. Nothing is stored or published.">
        <div className="grid gap-4 lg:grid-cols-2">
          <div className="flex flex-col gap-2">
            <span className="label">Sample payload</span>
            <Textarea className="h-72" value={payload} onChange={(e) => setPayload(e.target.value)} spellCheck={false} />
            {parseError && <span className="text-xs text-crit">{parseError}</span>}
            <Button variant="primary" onClick={() => m.mutate()} loading={m.isPending} disabled={!!parseError}><FlaskConical /> Run test</Button>
          </div>
          <div className="flex min-w-0 flex-col gap-2">
            <span className="label">Result</span>
            <ErrorBox error={m.error} />
            {m.data ? <JsonView value={m.data} className="[&_pre]:max-h-[330px]" /> : <div className="flex h-72 items-center justify-center rounded-md border border-dashed border-line text-sm text-fg-4">The normalized alert appears here</div>}
          </div>
        </div>
      </DialogContent>
    </Dialog>
  );
}
