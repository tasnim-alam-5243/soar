import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "react-router-dom";
import { toast } from "sonner";
import { Briefcase, Search } from "lucide-react";
import { api, unwrap, Problem, type Alert, type AlertPatch } from "@/lib/api";
import { VERDICTS, verdictName } from "@/lib/severity";
import { cn, pool } from "@/lib/utils";
import { Dialog, DialogContent } from "./ui/dialog";
import { Button } from "./ui/button";
import { FormField, Input, Select } from "./ui/input";
import { ErrorBox, StatusBadge, Time } from "./common";

// Alert triage shared by the list (bulk) and the detail panel (one alert).

const reason = (e: unknown) => (e instanceof Problem ? e.errors?.[0]?.message ?? e.detail ?? e.title : e instanceof Error ? e.message : String(e));

export function useTriage() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async ({ ids, patch }: { ids: string[]; patch: AlertPatch; label: string }) => {
      const res = await pool(ids, 4, (id) => unwrap(api().PATCH("/alerts/{id}", { params: { path: { id } }, body: patch })));
      return { ids, res };
    },
    onSuccess: ({ ids, res }, { label }) => {
      for (const r of res) if (r.status === "fulfilled") qc.setQueryData(["alert", r.value.id], r.value);
      qc.invalidateQueries({ queryKey: ["alerts"] });
      qc.invalidateQueries({ queryKey: ["dashboard"] });
      const failed = res.map((r, i) => ({ r, id: ids[i] })).filter((x) => x.r.status === "rejected");
      const ok = ids.length - failed.length;
      if (!failed.length) {
        toast.success(ids.length === 1 ? label : `${label}: ${ok} alerts`);
      } else {
        toast.error(`${label}: ${ok} of ${ids.length} updated`, {
          description: failed.slice(0, 3).map((f) => `${f.id}: ${reason((f.r as PromiseRejectedResult).reason)}`).join("\n"),
          duration: 10_000,
        });
      }
    },
    onError: (e) => toast.error("Triage failed", { description: reason(e) }),
  });
}

/** CloseDialog asks for the verdict every closed alert must carry. */
export function CloseDialog({ count, onClose, onConfirm, busy }: { count: number; onClose: () => void; onConfirm: (verdict: string) => void; busy?: boolean }) {
  const [verdict, setVerdict] = useState<string>("false_positive");
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent
        title={count === 1 ? "Close alert" : `Close ${count} alerts`}
        description="Closed alerts must carry a verdict."
        footer={
          <>
            <Button variant="ghost" onClick={onClose}>Cancel</Button>
            <Button variant="primary" loading={busy} onClick={() => onConfirm(verdict)}>Close {count === 1 ? "alert" : `${count} alerts`}</Button>
          </>
        }
      >
        <div role="radiogroup" className="grid grid-cols-2 gap-2">
          {VERDICTS.map((v, i) => (
            <button
              key={v}
              type="button"
              role="radio"
              aria-checked={verdict === v}
              autoFocus={i === 1}
              onClick={() => setVerdict(v)}
              onDoubleClick={() => onConfirm(v)}
              className={cn(
                "flex flex-col items-start gap-0.5 rounded-lg border px-3 py-2.5 text-left transition-colors",
                verdict === v ? "border-accent bg-accent/10" : "border-line-strong hover:bg-hover",
              )}
            >
              <span className="text-sm font-medium">{verdictName[v]}</span>
              <span className="text-xs text-fg-3">
                {v === "true_positive" ? "Real threat, handled" : v === "false_positive" ? "Detection fired wrongly" : v === "benign" ? "Real activity, no harm" : "Could not be determined"}
              </span>
            </button>
          ))}
        </div>
      </DialogContent>
    </Dialog>
  );
}

/** CaseDialog opens a new case from alerts or links them to an existing one. */
export function CaseDialog({ alerts, onClose }: { alerts: Alert[]; onClose: () => void }) {
  const nav = useNavigate();
  const qc = useQueryClient();
  const [mode, setMode] = useState<"new" | "link">("new");
  const [title, setTitle] = useState(alerts.length === 1 ? alerts[0].title : `${alerts[0]?.title ?? "Alerts"} and ${alerts.length - 1} more`);
  const [severity, setSeverity] = useState(Math.max(...alerts.map((a) => a.severity), 1));
  const [q, setQ] = useState("");
  const [picked, setPicked] = useState<string>();
  const cases = useQuery({
    queryKey: ["cases", "picker", q],
    enabled: mode === "link",
    queryFn: () => unwrap(api().GET("/cases", { params: { query: { status: "open,in_progress", limit: 20, ...(q.trim() ? { q: q.trim() } : {}) } } })),
  });
  const m = useMutation({
    mutationFn: async () => {
      if (mode === "new") {
        if (alerts.length === 1) return unwrap(api().POST("/alerts/{id}/case", { params: { path: { id: alerts[0].id } }, body: { title } }));
        return unwrap(api().POST("/cases", { body: { title, severity, alert_ids: alerts.map((a) => a.id) } }));
      }
      if (alerts.length === 1) return unwrap(api().POST("/alerts/{id}/case", { params: { path: { id: alerts[0].id } }, body: { case_id: picked } }));
      return unwrap(api().POST("/cases/{id}/alerts", { params: { path: { id: picked! } }, body: { alert_ids: alerts.map((a) => a.id) } }));
    },
    onSuccess: (c) => {
      qc.invalidateQueries({ queryKey: ["alerts"] });
      qc.invalidateQueries({ queryKey: ["cases"] });
      toast.success(mode === "new" ? "Case opened" : "Alerts linked", { action: { label: "Open case", onClick: () => nav(`/cases/${c.id}`) } });
      onClose();
    },
  });
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent
        title={alerts.length === 1 ? "Case for this alert" : `Case for ${alerts.length} alerts`}
        description="Linked alerts move to In progress; closing the case closes them with its verdict."
        footer={
          <>
            <Button variant="ghost" onClick={onClose}>Cancel</Button>
            <Button variant="primary" loading={m.isPending} disabled={mode === "new" ? !title.trim() : !picked} onClick={() => m.mutate()}>
              <Briefcase /> {mode === "new" ? "Open case" : "Link to case"}
            </Button>
          </>
        }
      >
        <div className="flex gap-1 rounded-md border border-line bg-bg/50 p-0.5">
          {(["new", "link"] as const).map((x) => (
            <button key={x} onClick={() => setMode(x)} className={cn("h-7 flex-1 rounded-[5px] text-xs font-medium", mode === x ? "bg-raised text-fg shadow-[0_0_0_1px_rgb(var(--line-strong))]" : "text-fg-3 hover:text-fg")}>
              {x === "new" ? "New case" : "Existing case"}
            </button>
          ))}
        </div>
        {mode === "new" ? (
          <>
            <FormField label="Title"><Input value={title} onChange={(e) => setTitle(e.target.value)} autoFocus /></FormField>
            {alerts.length > 1 && (
              <FormField label="Severity" hint="Defaults to the highest severity among the alerts.">
                <Select value={severity} onChange={(e) => setSeverity(Number(e.target.value))}>
                  {[5, 4, 3, 2, 1].map((s) => <option key={s} value={s}>{s} · {["", "Info", "Low", "Medium", "High", "Critical"][s]}</option>)}
                </Select>
              </FormField>
            )}
          </>
        ) : (
          <div className="flex flex-col gap-2">
            <Input icon={<Search />} placeholder="Search open cases" value={q} onChange={(e) => setQ(e.target.value)} autoFocus />
            <div className="max-h-64 overflow-y-auto rounded-md border border-line">
              {(cases.data?.items ?? []).map((c) => (
                <button
                  key={c.id}
                  onClick={() => setPicked(c.id)}
                  onDoubleClick={() => { setPicked(c.id); m.mutate(); }}
                  className={cn("flex w-full items-center gap-3 border-b border-line px-3 py-2 text-left last:border-0", picked === c.id ? "bg-accent/10" : "hover:bg-hover")}
                >
                  <StatusBadge status={c.status} iconOnly />
                  <span className="min-w-0 flex-1 truncate text-sm">{c.title}</span>
                  <span className="text-xs text-fg-3"><Time iso={c.created_at} /></span>
                </button>
              ))}
              {cases.data && !cases.data.items.length && <div className="px-3 py-6 text-center text-sm text-fg-3">No open cases match</div>}
            </div>
          </div>
        )}
        <ErrorBox error={m.error} />
      </DialogContent>
    </Dialog>
  );
}
