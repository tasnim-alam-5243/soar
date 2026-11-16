import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { useVirtualizer } from "@tanstack/react-virtual";
import { Link, useSearchParams } from "react-router-dom";
import {
  Bell, Bookmark, BookmarkPlus, Briefcase, Check, ChevronDown, CircleCheck, Columns3, ExternalLink, Eye, Filter, Search, ShieldAlert,
  Trash2, UserRound, X,
} from "lucide-react";
import { api, unwrap, type Alert } from "@/lib/api";
import { humanize, num } from "@/lib/format";
import { useHotkeys } from "@/lib/hotkeys";
import { requires, usePerms } from "@/lib/perms";
import { usePrefs } from "@/lib/prefs";
import { OPEN_ALERT_STATUSES, SEVERITIES, sevColor, sevName, verdictName } from "@/lib/severity";
import { useFlash, useLive } from "@/lib/stream";
import { cn } from "@/lib/utils";
import { AssigneePicker } from "@/components/AssigneePicker";
import { CaseDialog, CloseDialog, useTriage } from "@/components/triage";
import {
  Avatar, CopyButton, Empty, ErrorBox, Field, JsonView, ObservableChip, Section, SeverityBadge, SeverityGlyph, SkeletonRows, StatusBadge, Time,
} from "@/components/common";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Checkbox, Input, Kbd, Segmented } from "@/components/ui/input";
import { Menu, MenuCheck, MenuContent, MenuItem, MenuLabel, MenuSeparator, MenuTrigger } from "@/components/ui/menu";
import { Dialog, DialogContent } from "@/components/ui/dialog";
import { Tip } from "@/components/ui/tooltip";

const STATUS_TABS = [
  { value: OPEN_ALERT_STATUSES, label: "Open" },
  { value: "new", label: "New" },
  { value: "acknowledged", label: "Acknowledged" },
  { value: "in_progress", label: "In progress" },
  { value: "closed", label: "Closed" },
  { value: "", label: "All" },
];

const COLUMNS: { key: string; label: string }[] = [
  { key: "source", label: "Source" },
  { key: "host", label: "Host" },
  { key: "count", label: "Count" },
  { key: "assignee", label: "Assignee" },
  { key: "status", label: "Status" },
  { key: "seen", label: "Last seen" },
];

export default function Alerts() {
  const [params, setParams] = useSearchParams();
  const { me, can } = usePerms();
  const prefs = usePrefs();
  const status = params.get("status") ?? OPEN_ALERT_STATUSES;
  const severity = params.get("severity") ?? "";
  const source = params.get("source_id") ?? "";
  const assigneeParam = params.get("assignee") ?? "";
  const assignee = assigneeParam === "me" ? me?.name ?? "" : assigneeParam;
  const qParam = params.get("q") ?? "";
  const from = params.get("from") ?? "";
  const openId = params.get("alert") ?? "";
  const [search, setSearch] = useState(qParam);
  const searchRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    prefs.markSeen("alerts");
    useLive.setState({ newAlerts: 0 });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  useEffect(() => setSearch(qParam), [qParam]);
  useEffect(() => {
    const t = setTimeout(() => {
      if (search.trim() !== qParam) update({ q: search.trim() || null });
    }, 300);
    return () => clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [search]);

  const update = useCallback(
    (p: Record<string, string | null>) => {
      setParams((cur) => {
        const n = new URLSearchParams(cur);
        for (const [k, v] of Object.entries(p)) {
          if (v === null || v === "") n.delete(k);
          else n.set(k, v);
        }
        if (n.get("status") === OPEN_ALERT_STATUSES) n.delete("status");
        return n;
      }, { replace: true });
    },
    [setParams],
  );
  // "All" is an explicit empty status: keep it in the URL as status= so it survives reloads
  const setStatus = (v: string) =>
    setParams((cur) => {
      const n = new URLSearchParams(cur);
      if (v === OPEN_ALERT_STATUSES) n.delete("status");
      else n.set("status", v);
      return n;
    }, { replace: true });

  const query = useMemo(
    () => ({
      limit: 100,
      sort: "-received_at" as const,
      ...(status ? { status } : {}),
      ...(severity ? { severity } : {}),
      ...(source ? { source_id: source } : {}),
      ...(assignee ? { assignee } : {}),
      ...(qParam ? { q: qParam } : {}),
      ...(from ? { from } : {}),
    }),
    [status, severity, source, assignee, qParam, from],
  );
  const list = useInfiniteQuery({
    queryKey: ["alerts", "list", query],
    enabled: assigneeParam !== "me" || !!me,
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) => unwrap(api().GET("/alerts", { params: { query: { ...query, ...(pageParam ? { cursor: pageParam } : {}) } } })),
    getNextPageParam: (last) => last.next_cursor || undefined,
  });
  const rows = useMemo(() => list.data?.pages.flatMap((p) => p.items) ?? [], [list.data]);

  // selection and focus
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [cursor, setCursor] = useState(0);
  useEffect(() => setSelected(new Set()), [query]);
  const focusRow = rows[cursor];
  const open = (id: string | null) => update({ alert: id });
  const selectedAlerts = rows.filter((a) => selected.has(a.id));
  const targets = selectedAlerts.length ? selectedAlerts : focusRow ? [focusRow] : [];

  const triage = useTriage();
  const [closing, setClosing] = useState<Alert[] | null>(null);
  const [casing, setCasing] = useState<Alert[] | null>(null);
  const canWrite = can("alerts:write");
  const act = (label: string, alerts: Alert[], patch: Parameters<typeof triage.mutate>[0]["patch"]) => {
    if (!alerts.length || !canWrite) return;
    triage.mutate({ ids: alerts.map((a) => a.id), patch, label }, { onSuccess: () => setSelected(new Set()) });
  };

  const parentRef = useRef<HTMLDivElement>(null);
  const v = useVirtualizer({
    count: rows.length + (list.hasNextPage ? 1 : 0),
    getScrollElement: () => parentRef.current,
    estimateSize: () => (prefs.density === "compact" ? 32 : 40),
    overscan: 12,
  });
  const items = v.getVirtualItems();
  useEffect(() => {
    const last = items[items.length - 1];
    if (last && last.index >= rows.length - 10 && list.hasNextPage && !list.isFetchingNextPage) void list.fetchNextPage();
  }, [items, rows.length, list]);
  useEffect(() => v.measure(), [prefs.density, v]);

  const move = (d: number) => {
    const n = Math.max(0, Math.min(rows.length - 1, cursor + d));
    setCursor(n);
    v.scrollToIndex(n, { align: "auto" });
    if (openId && rows[n]) open(rows[n].id);
  };
  useHotkeys(
    {
      j: () => move(1),
      k: () => move(-1),
      ArrowDown: () => move(1),
      ArrowUp: () => move(-1),
      Enter: () => focusRow && open(focusRow.id),
      x: () => focusRow && setSelected((s) => toggleSet(s, focusRow.id)),
      "/": () => searchRef.current?.focus(),
      Escape: () => (openId ? open(null) : setSelected(new Set())),
      a: () => act("Acknowledged", targets, { status: "acknowledged" }),
      i: () => me && act(`Assigned to ${me.name}`, targets, { assignee: me.name }),
      c: () => canWrite && targets.length && setClosing(targets),
      o: () => can("cases:write") && targets.length && setCasing(targets),
    },
    [cursor, rows, openId, targets, me],
  );

  const sevSet = new Set(severity ? severity.split(",") : []);
  const filtersActive = !!(severity || source || assigneeParam || qParam || from);
  // with the detail panel open the list is narrower: host and count make room for the rest
  const cols = openId ? { ...prefs.alertColumns, host: false, count: false } : prefs.alertColumns;
  const grid = gridTemplate(cols);
  const allChecked = rows.length > 0 && selected.size === rows.length;
  const views = prefs.savedViews.alerts ?? [];

  return (
    <div className="flex h-full min-h-0">
      <div className="flex min-w-0 flex-1 flex-col">
        <div className="flex flex-col gap-3 border-b border-line px-6 pb-3 pt-5">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div className="flex items-center gap-3">
              <h1 className="text-lg font-semibold tracking-[-0.01em]">Alerts</h1>
              <span className="tabular text-sm text-fg-3">{rows.length}{list.hasNextPage ? "+" : ""} shown</span>
            </div>
            <div className="flex items-center gap-2">
              <SavedViews views={views} current={Object.fromEntries(params)} onApply={(p) => setParams(p, { replace: true })} />
              <ColumnsMenu />
            </div>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <Segmented value={status} onChange={setStatus} options={STATUS_TABS} />
            <Input
              ref={searchRef}
              icon={<Search />}
              placeholder="Search title, host, IP, user, tag…"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              onKeyDown={(e) => e.key === "Escape" && (e.currentTarget.blur(), setSearch(""))}
              className="w-72"
              trailing={<Kbd>/</Kbd>}
            />
            <Menu>
              <MenuTrigger asChild>
                <Button size="md" variant={sevSet.size ? "secondary" : "ghost"} className={cn(!sevSet.size && "border border-dashed border-line-strong")}>
                  <Filter /> Severity
                  {sevSet.size > 0 && (
                    <span className="flex items-center gap-0.5">
                      {[...sevSet].sort().reverse().map((s) => <span key={s} className="size-2 rounded-full" style={{ background: sevColor(Number(s)) }} />)}
                    </span>
                  )}
                  <ChevronDown className="size-3.5 text-fg-3" />
                </Button>
              </MenuTrigger>
              <MenuContent align="start">
                {SEVERITIES.map((s) => (
                  <MenuCheck key={s} checked={sevSet.has(String(s))} onSelect={(e) => e.preventDefault()} onCheckedChange={() => update({ severity: [...toggleSet(sevSet, String(s))].join(",") || null })}>
                    <SeverityBadge severity={s} />
                  </MenuCheck>
                ))}
              </MenuContent>
            </Menu>
            <Button
              size="md"
              variant={assigneeParam === "me" ? "secondary" : "ghost"}
              className={cn(assigneeParam !== "me" && "border border-dashed border-line-strong")}
              onClick={() => update({ assignee: assigneeParam === "me" ? null : "me" })}
              aria-pressed={assigneeParam === "me"}
            >
              <UserRound /> Assigned to me
            </Button>
            {source && <FilterChip label="Source" value={source} onClear={() => update({ source_id: null })} />}
            {assigneeParam && assigneeParam !== "me" && <FilterChip label="Assignee" value={assigneeParam} onClear={() => update({ assignee: null })} />}
            {from && <FilterChip label="Since" value={<Time iso={from} mode="absolute" />} onClear={() => update({ from: null })} />}
            {filtersActive && (
              <Button size="sm" variant="ghost" onClick={() => setParams(status !== OPEN_ALERT_STATUSES ? { status } : {}, { replace: true })}>
                Clear filters
              </Button>
            )}
          </div>
        </div>

        {/* header */}
        <div className="grid items-center gap-3 border-b border-line bg-panel px-6 py-2 text-2xs font-medium uppercase tracking-[0.05em] text-fg-3" style={{ gridTemplateColumns: grid }}>
          <Checkbox checked={allChecked} indeterminate={selected.size > 0 && !allChecked} onChange={(c) => setSelected(c ? new Set(rows.map((r) => r.id)) : new Set())} aria-label="Select all loaded alerts" />
          <span>Sev</span>
          <span>Alert</span>
          {cols.source && <span>Source</span>}
          {cols.host && <span>Host</span>}
          {cols.count && <span className="text-right">Count</span>}
          {cols.assignee && <span className="text-center">Owner</span>}
          {cols.status && <span>Status</span>}
          {cols.seen && <span className="text-right">Last seen</span>}
        </div>

        <div ref={parentRef} className="relative min-h-0 flex-1 overflow-auto" role="grid" aria-label="Alerts" aria-rowcount={rows.length}>
          {list.isLoading ? (
            <SkeletonRows rows={14} />
          ) : list.error ? (
            <div className="p-6"><ErrorBox error={list.error} /></div>
          ) : !rows.length ? (
            <Empty
              icon={filtersActive || status ? <Filter /> : <ShieldAlert />}
              title={filtersActive ? "No alerts match these filters" : status === OPEN_ALERT_STATUSES ? "No open alerts" : "No alerts"}
              text={filtersActive ? "Try a wider search or clear the filters." : "New detections from your sources appear here as they arrive."}
            />
          ) : (
            <div style={{ height: v.getTotalSize() }} className="relative w-full">
              {items.map((it) => {
                const a = rows[it.index];
                if (!a) {
                  return <div key="more" className="absolute left-0 w-full px-6 py-3 text-center text-xs text-fg-3" style={{ transform: `translateY(${it.start}px)` }}>Loading more…</div>;
                }
                return (
                  <AlertRow
                    key={a.id}
                    a={a}
                    grid={grid}
                    cols={cols}
                    top={it.start}
                    focused={it.index === cursor}
                    open={a.id === openId}
                    checked={selected.has(a.id)}
                    onCheck={() => setSelected((s) => toggleSet(s, a.id))}
                    onClick={() => { setCursor(it.index); open(a.id); }}
                  />
                );
              })}
            </div>
          )}
        </div>

        {selected.size > 0 && (
          <BulkBar
            count={selected.size}
            busy={triage.isPending}
            canWrite={canWrite}
            canCase={can("cases:write")}
            me={me?.name}
            onAck={() => act("Acknowledged", selectedAlerts, { status: "acknowledged" })}
            onStart={() => act("Moved to in progress", selectedAlerts, { status: "in_progress" })}
            onAssign={(n) => act(n ? `Assigned to ${n}` : "Unassigned", selectedAlerts, { assignee: n })}
            onClose={() => setClosing(selectedAlerts)}
            onCase={() => setCasing(selectedAlerts)}
            onClear={() => setSelected(new Set())}
          />
        )}
      </div>

      {openId && <AlertPanel id={openId} seed={rows.find((r) => r.id === openId)} onClose={() => open(null)} onCloseAlert={(a) => setClosing([a])} onCase={(a) => setCasing([a])} />}

      {closing && (
        <CloseDialog
          count={closing.length}
          busy={triage.isPending}
          onClose={() => setClosing(null)}
          onConfirm={(verdict) => {
            act(`Closed as ${verdictName[verdict].toLowerCase()}`, closing, { status: "closed", verdict: verdict as never });
            setClosing(null);
          }}
        />
      )}
      {casing && <CaseDialog alerts={casing} onClose={() => { setCasing(null); setSelected(new Set()); }} />}
    </div>
  );
}

function gridTemplate(cols: Record<string, boolean>) {
  return [
    "16px", "28px", "minmax(240px,1fr)",
    cols.source && "minmax(90px,140px)",
    cols.host && "minmax(80px,130px)",
    cols.count && "52px",
    cols.assignee && "48px",
    cols.status && "112px",
    cols.seen && "76px",
  ].filter(Boolean).join(" ");
}

function toggleSet<T>(s: Set<T>, v: T): Set<T> {
  const n = new Set(s);
  if (n.has(v)) n.delete(v);
  else n.add(v);
  return n;
}

function AlertRow({ a, grid, cols, top, focused, open, checked, onCheck, onClick }: {
  a: Alert;
  grid: string;
  cols: Record<string, boolean>;
  top: number;
  focused: boolean;
  open: boolean;
  checked: boolean;
  onCheck: () => void;
  onClick: () => void;
}) {
  const flash = useFlash(a.id);
  const closed = a.status === "closed";
  return (
    <div
      role="row"
      aria-selected={open}
      onClick={onClick}
      className={cn(
        "absolute left-0 grid h-[var(--row-h)] w-full cursor-default items-center gap-3 border-b border-line px-6 text-sm transition-colors",
        open ? "bg-accent/[0.08]" : checked ? "bg-hover" : "hover:bg-hover/60",
        focused && "shadow-[inset_2px_0_0_rgb(var(--accent))]",
      )}
      style={{ transform: `translateY(${top}px)`, gridTemplateColumns: grid }}
    >
      {flash && <span key={flash} className="pointer-events-none absolute inset-0 animate-flash" aria-hidden />}
      <Checkbox checked={checked} onChange={onCheck} aria-label={`Select ${a.title}`} />
      <Tip content={`${sevName[a.severity] ?? a.severity} (${a.severity})`}>
        <span className="flex"><SeverityGlyph severity={a.severity} /></span>
      </Tip>
      <div className="flex min-w-0 items-center gap-2">
        <span className={cn("truncate", closed ? "text-fg-3" : a.status === "new" || !a.status ? "font-medium text-fg" : "text-fg")}>{a.title}</span>
        {a.case_id && <Tip content="Linked to a case"><Briefcase className="size-3.5 shrink-0 text-fg-3" /></Tip>}
        {a.verdict && closed && <Badge tone="outline" className="shrink-0">{verdictName[a.verdict] ?? a.verdict}</Badge>}
      </div>
      {cols.source && <span className="truncate font-mono text-xs text-fg-3">{a.source_id}</span>}
      {cols.host && <span className="truncate font-mono text-xs text-fg-2">{(a.entities as { hosts?: string[] } | undefined)?.hosts?.[0] ?? "—"}</span>}
      {cols.count && <span className={cn("tabular text-right text-xs", a.occurrence_count > 1 ? "font-medium text-fg-2" : "text-fg-4")}>{num(a.occurrence_count)}</span>}
      {cols.assignee && <span className="flex justify-center"><Avatar name={a.assignee} size={20} /></span>}
      {cols.status && <StatusBadge status={a.status || "new"} />}
      {cols.seen && <span className="text-right text-xs text-fg-3"><Time iso={a.last_seen ?? a.received_at} /></span>}
    </div>
  );
}

function FilterChip({ label, value, onClear }: { label: string; value: React.ReactNode; onClear: () => void }) {
  return (
    <span className="inline-flex h-8 items-center gap-1.5 rounded-md border border-line-strong bg-raised pl-2.5 pr-1 text-xs">
      <span className="text-fg-3">{label}</span>
      <span className="max-w-[180px] truncate font-medium">{value}</span>
      <button onClick={onClear} className="rounded p-0.5 text-fg-3 hover:bg-hover hover:text-fg" aria-label={`Clear ${label}`}><X className="size-3.5" /></button>
    </span>
  );
}

function ColumnsMenu() {
  const cols = usePrefs((s) => s.alertColumns);
  const set = usePrefs((s) => s.set);
  return (
    <Menu>
      <MenuTrigger asChild><Button size="icon" variant="ghost" tip="Columns" aria-label="Columns"><Columns3 /></Button></MenuTrigger>
      <MenuContent>
        <MenuLabel>Columns</MenuLabel>
        {COLUMNS.map((c) => (
          <MenuCheck key={c.key} checked={cols[c.key] !== false} onSelect={(e) => e.preventDefault()} onCheckedChange={(v) => set({ alertColumns: { ...cols, [c.key]: !!v } })}>
            {c.label}
          </MenuCheck>
        ))}
      </MenuContent>
    </Menu>
  );
}

function SavedViews({ views, current, onApply }: { views: { name: string; params: Record<string, string> }[]; current: Record<string, string>; onApply: (p: Record<string, string>) => void }) {
  const save = usePrefs((s) => s.saveView);
  const del = usePrefs((s) => s.deleteView);
  const [naming, setNaming] = useState(false);
  const [name, setName] = useState("");
  const { alert: _a, ...params } = current;
  void _a;
  return (
    <>
      <Menu>
        <MenuTrigger asChild>
          <Button size="md" variant="ghost"><Bookmark /> Views{views.length > 0 && <span className="text-fg-4">{views.length}</span>}</Button>
        </MenuTrigger>
        <MenuContent className="w-60">
          <MenuLabel>Saved views (this browser)</MenuLabel>
          {views.map((v) => (
            <MenuItem key={v.name} icon={<Eye />} onSelect={() => onApply(v.params)}>
              <span className="flex items-center justify-between gap-2">
                {v.name}
                <button className="rounded p-0.5 text-fg-4 hover:text-crit" onClick={(e) => { e.stopPropagation(); del("alerts", v.name); }} aria-label={`Delete view ${v.name}`}><Trash2 className="size-3.5" /></button>
              </span>
            </MenuItem>
          ))}
          {!views.length && <div className="px-2 py-2 text-xs text-fg-3">Save the current filters to come back to them in one click.</div>}
          <MenuSeparator />
          <MenuItem icon={<BookmarkPlus />} onSelect={() => setTimeout(() => setNaming(true), 0)}>Save current filters…</MenuItem>
        </MenuContent>
      </Menu>
      <Dialog open={naming} onOpenChange={setNaming}>
        {naming && (
          <DialogContent title="Save view" description="Saves the current filters in this browser.">
            <form className="flex flex-col gap-3" onSubmit={(e) => { e.preventDefault(); if (name.trim()) { save("alerts", { name: name.trim(), params }); setName(""); setNaming(false); } }}>
              <Input autoFocus value={name} onChange={(e) => setName(e.target.value)} placeholder="Critical on web servers" aria-label="View name" />
              <Button type="submit" variant="primary" disabled={!name.trim()}><BookmarkPlus /> Save view</Button>
            </form>
          </DialogContent>
        )}
      </Dialog>
    </>
  );
}

function BulkBar({ count, busy, canWrite, canCase, me, onAck, onStart, onAssign, onClose, onCase, onClear }: {
  count: number;
  busy: boolean;
  canWrite: boolean;
  canCase: boolean;
  me?: string;
  onAck: () => void;
  onStart: () => void;
  onAssign: (n: string) => void;
  onClose: () => void;
  onCase: () => void;
  onClear: () => void;
}) {
  const no = canWrite ? undefined : requires("alerts:write");
  return (
    <div className="pointer-events-none absolute inset-x-0 bottom-5 z-20 flex justify-center px-4">
      <div className="pointer-events-auto flex items-center gap-1 rounded-xl border border-line-strong bg-raised p-1.5 shadow-pop animate-pop-in">
        <span className="tabular px-2 text-sm font-medium">{count} selected</span>
        <span className="mx-1 h-5 w-px bg-line" />
        <Button size="sm" variant="ghost" onClick={onAck} disabled={!canWrite || busy} tip={no ?? "Acknowledge (a)"}><Check /> Acknowledge</Button>
        <Button size="sm" variant="ghost" onClick={onStart} disabled={!canWrite || busy} tip={no}>Start</Button>
        <AssigneePicker onChange={onAssign} disabled={!canWrite || busy}>
          <Button size="sm" variant="ghost" disabled={!canWrite || busy} tip={no ?? (me ? "Assign (i assigns to you)" : undefined)}><UserRound /> Assign</Button>
        </AssigneePicker>
        <Button size="sm" variant="ghost" onClick={onClose} disabled={!canWrite || busy} tip={no ?? "Close with a verdict (c)"}><CircleCheck /> Close</Button>
        <Button size="sm" variant="ghost" onClick={onCase} disabled={!canCase || busy} tip={canCase ? "Open or link a case (o)" : requires("cases:write")}><Briefcase /> Case</Button>
        <span className="mx-1 h-5 w-px bg-line" />
        <Button size="icon-sm" variant="ghost" onClick={onClear} tip="Clear selection (Esc)" aria-label="Clear selection"><X /></Button>
      </div>
    </div>
  );
}

// ---------- detail panel ----------

function AlertPanel({ id, seed, onClose, onCloseAlert, onCase }: { id: string; seed?: Alert; onClose: () => void; onCloseAlert: (a: Alert) => void; onCase: (a: Alert) => void }) {
  const { can } = usePerms();
  const q = useQuery({ queryKey: ["alert", id], queryFn: () => unwrap(api().GET("/alerts/{id}", { params: { path: { id } } })), placeholderData: seed });
  const triage = useTriage();
  const a = q.data;
  const canWrite = can("alerts:write");
  const no = canWrite ? undefined : requires("alerts:write");
  const set = (label: string, patch: Parameters<typeof triage.mutate>[0]["patch"]) => a && triage.mutate({ ids: [a.id], patch, label });
  const ent = (a?.entities ?? {}) as { hosts?: string[]; users?: string[]; accounts?: string[] };
  return (
    <aside className="flex w-[min(520px,46vw)] shrink-0 flex-col border-l border-line bg-panel animate-slide-in" aria-label="Alert details">
      <div className="flex h-12 items-center gap-2 border-b border-line px-4">
        <Bell className="size-4 text-fg-3" />
        <span className="font-mono text-xs text-fg-3">{id}</span>
        <CopyButton value={id} label="Copy id" />
        <div className="ml-auto flex items-center gap-1">
          <span className="mr-1 hidden text-2xs text-fg-4 lg:inline">j/k to move</span>
          <Button size="icon-sm" variant="ghost" onClick={onClose} tip="Close (Esc)" aria-label="Close panel"><X /></Button>
        </div>
      </div>
      {q.error ? <div className="p-4"><ErrorBox error={q.error} /></div> : !a ? <SkeletonRows rows={6} /> : (
        <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
          <div className="flex flex-col gap-3 border-b border-line p-5">
            <div className="flex flex-wrap items-center gap-3">
              <SeverityBadge severity={a.severity} />
              <StatusBadge status={a.status || "new"} />
              {a.verdict && <Badge tone="outline">{verdictName[a.verdict] ?? humanize(a.verdict)}</Badge>}
            </div>
            <h2 className="text-[17px] font-semibold leading-snug tracking-[-0.01em]">{a.title}</h2>
            <div className="flex flex-wrap items-center gap-1.5">
              {a.status !== "acknowledged" && a.status !== "closed" && a.status !== "in_progress" && (
                <Button size="sm" variant="primary" onClick={() => set("Acknowledged", { status: "acknowledged" })} disabled={!canWrite} loading={triage.isPending} tip={no ?? "a"}><Check /> Acknowledge</Button>
              )}
              {a.status !== "in_progress" && a.status !== "closed" && (
                <Button size="sm" onClick={() => set("Moved to in progress", { status: "in_progress" })} disabled={!canWrite} tip={no}>Start</Button>
              )}
              {a.status !== "closed" ? (
                <Button size="sm" onClick={() => onCloseAlert(a)} disabled={!canWrite} tip={no ?? "c"}><CircleCheck /> Close…</Button>
              ) : (
                <Button size="sm" onClick={() => set("Reopened", { status: "new" })} disabled={!canWrite} tip={no}>Reopen</Button>
              )}
              <AssigneePicker value={a.assignee} onChange={(n) => set(n ? `Assigned to ${n}` : "Unassigned", { assignee: n })} disabled={!canWrite}>
                <Button size="sm" disabled={!canWrite} tip={no ?? (a.assignee ? `Assigned to ${a.assignee}` : "i assigns to you")}>
                  <Avatar name={a.assignee} size={16} /> {a.assignee || "Assign"}
                </Button>
              </AssigneePicker>
              {a.case_id ? (
                <Button size="sm" asChild><Link to={`/cases/${a.case_id}`}><Briefcase /> Open case</Link></Button>
              ) : (
                <Button size="sm" onClick={() => onCase(a)} disabled={!can("cases:write")} tip={can("cases:write") ? "o" : requires("cases:write")}><Briefcase /> Case…</Button>
              )}
            </div>
          </div>

          <div className="flex flex-col gap-6 p-5">
            <div className="flex flex-col">
              <Field label="Source"><span className="font-mono text-xs">{a.source_id}</span>{a.system && a.system !== a.source_id && <span className="text-fg-3"> · {a.system}</span>}</Field>
              <Field label="Received"><Time iso={a.received_at} mode="absolute" /> <span className="text-fg-3">(<Time iso={a.received_at} />)</span></Field>
              {a.occurred_at && <Field label="Occurred"><Time iso={a.occurred_at} mode="absolute" /></Field>}
              <Field label="Occurrences">
                <span className="tabular">{num(a.occurrence_count)}</span>
                {a.occurrence_count > 1 && <span className="text-fg-3"> · last <Time iso={a.last_seen} /></span>}
              </Field>
              <Field label="Vendor severity"><span className="font-mono text-xs">{a.original_severity ?? "—"}</span></Field>
              {a.category && <Field label="Category">{a.category}</Field>}
              {a.external_id && (
                <Field label="External id">
                  <span className="inline-flex items-center gap-1 font-mono text-xs">
                    {a.external_id}
                    <CopyButton value={a.external_id} />
                    {a.external_url && /^https?:\/\//.test(a.external_url) && <a href={a.external_url} target="_blank" rel="noreferrer noopener" className="rounded p-1 text-fg-3 hover:bg-hover hover:text-fg" aria-label="Open in source"><ExternalLink className="size-3.5" /></a>}
                  </span>
                </Field>
              )}
              {a.case_id && <Field label="Case"><Link to={`/cases/${a.case_id}`} className="font-mono text-xs text-accent hover:underline">{a.case_id}</Link></Field>}
            </div>

            {(a.description || a.summary) && (
              <Section title="Details">
                {a.summary && <p className="text-sm text-fg-2">{a.summary}</p>}
                {a.description && <pre className="max-h-48 overflow-auto whitespace-pre-wrap break-words rounded-md border border-line bg-bg/60 p-3 font-mono text-[11.5px] leading-5 text-fg-2">{a.description}</pre>}
              </Section>
            )}

            {(a.observables?.length ?? 0) > 0 && (
              <Section title={`Observables · ${a.observables!.length}`}>
                <div className="flex flex-wrap gap-1.5">{a.observables!.map((o, i) => <ObservableChip key={i} type={o.type} value={o.value} role={o.role} />)}</div>
              </Section>
            )}

            {(ent.hosts?.length || ent.users?.length) ? (
              <Section title="Entities">
                <div className="flex flex-col">
                  {ent.hosts?.length ? <Field label="Hosts"><span className="font-mono text-xs">{ent.hosts.join(", ")}</span></Field> : null}
                  {ent.users?.length ? <Field label="Users"><span className="font-mono text-xs">{ent.users.join(", ")}</span></Field> : null}
                </div>
              </Section>
            ) : null}

            {a.mitre && ((a.mitre.technique_ids?.length ?? 0) > 0 || (a.mitre.tactics?.length ?? 0) > 0) && (
              <Section title="MITRE ATT&CK">
                <div className="flex flex-wrap gap-1.5">
                  {(a.mitre.tactics ?? []).map((t) => <Badge key={t} tone="accent">{t}</Badge>)}
                  {(a.mitre.technique_ids ?? []).map((t, i) => (
                    <a key={t} href={`https://attack.mitre.org/techniques/${t.replace(".", "/")}/`} target="_blank" rel="noreferrer noopener">
                      <Badge tone="outline" className="hover:border-accent"><span className="font-mono">{t}</span>{a.mitre?.techniques?.[i] ? ` · ${a.mitre.techniques[i]}` : ""}</Badge>
                    </a>
                  ))}
                </div>
              </Section>
            )}

            {(a.tags?.length ?? 0) > 0 && (
              <Section title="Tags">
                <div className="flex flex-wrap gap-1.5">{a.tags!.map((t) => <Badge key={t}>{t}</Badge>)}</div>
              </Section>
            )}

            {a.ext && Object.keys(a.ext).length > 0 && (
              <Section title="Vendor fields">
                <details className="group">
                  <summary className="cursor-pointer list-none text-xs text-fg-3 hover:text-fg"><span className="group-open:hidden">Show raw fields</span><span className="hidden group-open:inline">Hide raw fields</span></summary>
                  <JsonView value={a.ext} className="mt-2" />
                </details>
              </Section>
            )}
          </div>
        </div>
      )}
    </aside>
  );
}
