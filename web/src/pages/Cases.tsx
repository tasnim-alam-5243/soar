import { useEffect, useMemo, useRef, useState } from "react";
import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { AlarmClock, Bell, Briefcase, ChevronDown, Filter, Plus, Search, UserRound } from "lucide-react";
import { api, unwrap, type Case } from "@/lib/api";
import { useHotkeys } from "@/lib/hotkeys";
import { requires, usePerms } from "@/lib/perms";
import { SEVERITIES, sevColor, verdictName } from "@/lib/severity";
import { useFlash } from "@/lib/stream";
import { cn } from "@/lib/utils";
import { Page } from "@/components/Layout";
import { Avatar, Empty, ErrorBox, PageHeader, SeverityBadge, SeverityGlyph, SkeletonRows, StatusBadge, Time } from "@/components/common";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Dialog, DialogContent } from "@/components/ui/dialog";
import { FormField, Input, Kbd, Segmented, Select, Textarea } from "@/components/ui/input";
import { Menu, MenuCheck, MenuContent, MenuTrigger } from "@/components/ui/menu";
import { Tip } from "@/components/ui/tooltip";

const OPEN = "open,in_progress";

export default function Cases() {
  const [params, setParams] = useSearchParams();
  const { me, can } = usePerms();
  const nav = useNavigate();
  const status = params.get("status") ?? OPEN;
  const severity = params.get("severity") ?? "";
  const assigneeParam = params.get("assignee") ?? "";
  const qParam = params.get("q") ?? "";
  const [search, setSearch] = useState(qParam);
  const [creating, setCreating] = useState(false);
  const searchRef = useRef<HTMLInputElement>(null);
  const update = (p: Record<string, string | null>) =>
    setParams((cur) => {
      const n = new URLSearchParams(cur);
      for (const [k, v] of Object.entries(p)) (v === null ? n.delete(k) : n.set(k, v));
      if (n.get("status") === OPEN) n.delete("status");
      return n;
    }, { replace: true });
  useEffect(() => {
    const t = setTimeout(() => search.trim() !== qParam && update({ q: search.trim() || null }), 300);
    return () => clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [search]);

  const assignee = assigneeParam === "me" ? me?.name : assigneeParam;
  const query = useMemo(
    () => ({ limit: 50, sort: "-created_at" as const, ...(status ? { status } : {}), ...(severity ? { severity } : {}), ...(assignee ? { assignee } : {}), ...(qParam ? { q: qParam } : {}) }),
    [status, severity, assignee, qParam],
  );
  const list = useInfiniteQuery({
    queryKey: ["cases", "list", query],
    enabled: assigneeParam !== "me" || !!me,
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) => unwrap(api().GET("/cases", { params: { query: { ...query, ...(pageParam ? { cursor: pageParam } : {}) } } })),
    getNextPageParam: (l) => l.next_cursor || undefined,
  });
  const rows = list.data?.pages.flatMap((p) => p.items) ?? [];
  const [cursor, setCursor] = useState(0);
  useHotkeys({
    j: () => setCursor((c) => Math.min(rows.length - 1, c + 1)),
    k: () => setCursor((c) => Math.max(0, c - 1)),
    Enter: () => rows[cursor] && nav(`/cases/${rows[cursor].id}`),
    "/": () => searchRef.current?.focus(),
    n: () => can("cases:write") && setCreating(true),
  }, [rows, cursor]);
  const sevSet = new Set(severity ? severity.split(",") : []);

  return (
    <Page>
      <PageHeader
        title="Cases"
        subtitle="Investigations: linked alerts, observables, tasks, evidence and the timeline."
        actions={<Button variant="primary" onClick={() => setCreating(true)} disabled={!can("cases:write")} tip={can("cases:write") ? "New case (n)" : requires("cases:write")}><Plus /> New case</Button>}
      >
        <div className="flex flex-wrap items-center gap-2">
          <Segmented
            value={status}
            onChange={(v) => setParams((cur) => { const n = new URLSearchParams(cur); v === OPEN ? n.delete("status") : n.set("status", v); return n; }, { replace: true })}
            options={[{ value: OPEN, label: "Active" }, { value: "open", label: "Open" }, { value: "in_progress", label: "In progress" }, { value: "closed", label: "Closed" }, { value: "", label: "All" }]}
          />
          <Input ref={searchRef} icon={<Search />} placeholder="Search cases" value={search} onChange={(e) => setSearch(e.target.value)} className="w-64" trailing={<Kbd>/</Kbd>} />
          <Menu>
            <MenuTrigger asChild>
              <Button variant={sevSet.size ? "secondary" : "ghost"} className={cn(!sevSet.size && "border border-dashed border-line-strong")}>
                <Filter /> Severity
                {sevSet.size > 0 && <span className="flex gap-0.5">{[...sevSet].sort().reverse().map((s) => <span key={s} className="size-2 rounded-full" style={{ background: sevColor(Number(s)) }} />)}</span>}
                <ChevronDown className="size-3.5 text-fg-3" />
              </Button>
            </MenuTrigger>
            <MenuContent align="start">
              {SEVERITIES.map((s) => (
                <MenuCheck key={s} checked={sevSet.has(String(s))} onSelect={(e) => e.preventDefault()} onCheckedChange={() => { const n = new Set(sevSet); n.has(String(s)) ? n.delete(String(s)) : n.add(String(s)); update({ severity: [...n].join(",") || null }); }}>
                  <SeverityBadge severity={s} />
                </MenuCheck>
              ))}
            </MenuContent>
          </Menu>
          <Button variant={assigneeParam === "me" ? "secondary" : "ghost"} className={cn(assigneeParam !== "me" && "border border-dashed border-line-strong")} onClick={() => update({ assignee: assigneeParam === "me" ? null : "me" })} aria-pressed={assigneeParam === "me"}>
            <UserRound /> My cases
          </Button>
        </div>
      </PageHeader>

      <Card className="overflow-hidden">
        <div className="grid grid-cols-[28px_minmax(240px,1fr)_120px_150px_70px_130px_90px] items-center gap-3 border-b border-line px-4 py-2 text-2xs font-medium uppercase tracking-[0.05em] text-fg-3">
          <span>Sev</span><span>Case</span><span>Status</span><span>Owner</span><span className="text-right">Alerts</span><span>SLA</span><span className="text-right">Opened</span>
        </div>
        {list.isLoading ? <SkeletonRows rows={10} /> : list.error ? <div className="p-4"><ErrorBox error={list.error} /></div> : !rows.length ? (
          <Empty icon={<Briefcase />} title={qParam || severity || assigneeParam ? "No cases match" : status === OPEN ? "No active cases" : "No cases"} text="Open a case from alerts on the Alerts page, or start one here." />
        ) : (
          <div role="table" aria-label="Cases">
            {rows.map((c, i) => <CaseRow key={c.id} c={c} focused={i === cursor} />)}
            {list.hasNextPage && (
              <div className="flex justify-center border-t border-line p-2">
                <Button variant="ghost" size="sm" loading={list.isFetchingNextPage} onClick={() => list.fetchNextPage()}>Load more</Button>
              </div>
            )}
          </div>
        )}
      </Card>
      {creating && <NewCase onClose={() => setCreating(false)} />}
    </Page>
  );
}

export function slaState(c: Case): { due?: string; breached: boolean; soon: boolean } {
  const sla = (c.sla ?? {}) as { due_at?: string; breached?: boolean };
  const due = sla.due_at;
  const left = due ? new Date(due).getTime() - Date.now() : Infinity;
  return { due, breached: !!sla.breached || (c.status !== "closed" && left < 0), soon: c.status !== "closed" && left >= 0 && left < 4 * 3600_000 };
}

function CaseRow({ c, focused }: { c: Case; focused: boolean }) {
  const flash = useFlash(c.id);
  const sla = slaState(c);
  return (
    <Link
      to={`/cases/${c.id}`}
      role="row"
      className={cn(
        "relative grid h-[var(--row-h)] grid-cols-[28px_minmax(240px,1fr)_120px_150px_70px_130px_90px] items-center gap-3 border-b border-line px-4 text-sm last:border-0 hover:bg-hover/60",
        focused && "shadow-[inset_2px_0_0_rgb(var(--accent))]",
      )}
    >
      {flash && <span key={flash} className="pointer-events-none absolute inset-0 animate-flash" aria-hidden />}
      <SeverityGlyph severity={c.severity} />
      <span className="flex min-w-0 items-center gap-2">
        <span className={cn("truncate", c.status === "closed" ? "text-fg-3" : "font-medium")}>{c.title}</span>
        {(c.tags ?? []).slice(0, 2).map((t) => <Badge key={t} className="shrink-0">{t}</Badge>)}
        {c.status === "closed" && c.verdict && <Badge tone="outline" className="shrink-0">{verdictName[c.verdict] ?? c.verdict}</Badge>}
      </span>
      <StatusBadge status={c.status} />
      <span className="flex min-w-0 items-center gap-2"><Avatar name={c.assignee} size={20} /><span className="truncate text-fg-2">{c.assignee || <span className="text-fg-4">Unassigned</span>}</span></span>
      <span className="tabular flex items-center justify-end gap-1 text-fg-2"><Bell className="size-3 text-fg-4" />{c.alert_ids?.length ?? 0}</span>
      <span className="text-xs">
        {!sla.due ? <span className="text-fg-4">—</span> : c.status === "closed" ? <span className="text-fg-3">met</span> : (
          <Tip content={`Due ${new Date(sla.due).toLocaleString()}`}>
            <span className={cn("inline-flex items-center gap-1 font-medium", sla.breached ? "text-crit" : sla.soon ? "text-warn" : "text-fg-2")}>
              <AlarmClock className="size-3.5" /> {sla.breached ? "Breached" : <Time iso={sla.due} />}
            </span>
          </Tip>
        )}
      </span>
      <span className="text-right text-xs text-fg-3"><Time iso={c.created_at} /></span>
    </Link>
  );
}

function NewCase({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient();
  const nav = useNavigate();
  const { me } = usePerms();
  const [title, setTitle] = useState("");
  const [summary, setSummary] = useState("");
  const [severity, setSeverity] = useState(3);
  const [mine, setMine] = useState(true);
  const m = useMutation({
    mutationFn: () => unwrap(api().POST("/cases", { body: { title, summary, severity, ...(mine && me?.name ? { assignee: me.name } : {}) } })),
    onSuccess: (c) => { qc.invalidateQueries({ queryKey: ["cases"] }); nav(`/cases/${c.id}`); },
  });
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent
        title="New case"
        footer={<><Button variant="ghost" onClick={onClose}>Cancel</Button><Button variant="primary" form="new-case" type="submit" disabled={!title.trim()} loading={m.isPending}>Create case</Button></>}
      >
        <form id="new-case" className="flex flex-col gap-4" onSubmit={(e) => { e.preventDefault(); if (title.trim()) m.mutate(); }}>
          <FormField label="Title"><Input value={title} onChange={(e) => setTitle(e.target.value)} placeholder="Phishing campaign against finance" autoFocus /></FormField>
          <FormField label="Severity">
            <Select value={severity} onChange={(e) => setSeverity(Number(e.target.value))}>
              {SEVERITIES.map((s) => <option key={s} value={s}>{s} · {["", "Info", "Low", "Medium", "High", "Critical"][s]}</option>)}
            </Select>
          </FormField>
          <FormField label="Summary" hint="What happened and what is known so far."><Textarea className="font-sans text-sm" value={summary} onChange={(e) => setSummary(e.target.value)} /></FormField>
          {me?.name && (
            <label className="flex items-center gap-2 text-sm text-fg-2">
              <input type="checkbox" checked={mine} onChange={(e) => setMine(e.target.checked)} className="accent-[rgb(var(--accent))]" /> Assign to me ({me.name})
            </label>
          )}
          <ErrorBox error={m.error} />
        </form>
      </DialogContent>
    </Dialog>
  );
}
