import { useEffect, useState } from "react";
import { Command } from "cmdk";
import * as D from "@radix-ui/react-dialog";
import { useQuery } from "@tanstack/react-query";
import { useNavigate } from "react-router-dom";
import { ArrowRight, Bell, Briefcase, Clock, Keyboard, LogOut, Moon, Rows3, Search, Sun } from "lucide-react";
import { api, unwrap } from "@/lib/api";
import { signOut } from "@/lib/auth";
import { usePerms } from "@/lib/perms";
import { usePrefs } from "@/lib/prefs";
import { NAV } from "./nav";
import { SeverityGlyph, StatusBadge } from "./common";

function useDebounced<T>(v: T, ms = 220) {
  const [d, setD] = useState(v);
  useEffect(() => {
    const t = setTimeout(() => setD(v), ms);
    return () => clearTimeout(t);
  }, [v, ms]);
  return d;
}

const idRoute: [RegExp, (id: string) => string, string][] = [
  [/^alt_/, (id) => `/alerts?alert=${id}`, "alert"],
  [/^cas_/, (id) => `/cases/${id}`, "case"],
  [/^exe_/, (id) => `/executions/${id}`, "execution"],
  [/^wf[l_]/, (id) => `/workflows/${id}`, "workflow"],
];

/** CommandPalette (⌘K): jump to pages, search alerts and cases, run quick actions. */
export function CommandPalette({ open, onOpenChange, onShortcuts }: { open: boolean; onOpenChange: (o: boolean) => void; onShortcuts: () => void }) {
  const nav = useNavigate();
  const { can, canAny } = usePerms();
  const prefs = usePrefs();
  const [q, setQ] = useState("");
  const dq = useDebounced(q.trim());
  useEffect(() => {
    if (!open) setQ("");
  }, [open]);

  const searching = dq.length >= 2;
  const alerts = useQuery({
    queryKey: ["palette", "alerts", dq],
    enabled: open && searching && can("alerts:read"),
    queryFn: () => unwrap(api().GET("/alerts", { params: { query: { q: dq, limit: 6 } } })),
    staleTime: 15_000,
  });
  const cases = useQuery({
    queryKey: ["palette", "cases", dq],
    enabled: open && searching && can("cases:read"),
    queryFn: () => unwrap(api().GET("/cases", { params: { query: { q: dq, limit: 6 } } })),
    staleTime: 15_000,
  });

  const go = (to: string) => {
    onOpenChange(false);
    nav(to);
  };
  const run = (fn: () => void) => {
    onOpenChange(false);
    fn();
  };
  const idMatch = idRoute.find(([re]) => re.test(q.trim()));

  return (
    <D.Root open={open} onOpenChange={onOpenChange}>
      <D.Portal>
        <D.Overlay className="fixed inset-0 z-50 bg-black/50 backdrop-blur-[2px] animate-fade-in" />
        <D.Content className="fixed left-1/2 top-[14vh] z-50 w-[calc(100vw-32px)] max-w-[620px] -translate-x-1/2 overflow-hidden rounded-xl border border-line-strong bg-raised shadow-pop animate-pop-in focus:outline-none">
          <D.Title className="sr-only">Command palette</D.Title>
          <D.Description className="sr-only">Search and run commands</D.Description>
          <Command label="Command palette" shouldFilter={!searching} loop className="flex flex-col">
            <div className="flex items-center gap-2 border-b border-line px-4">
              <Search className="size-4 text-fg-3" />
              <Command.Input
                value={q}
                onValueChange={setQ}
                placeholder="Search alerts and cases, jump to a page, or type an id…"
                className="h-12 flex-1 bg-transparent text-[15px] text-fg placeholder:text-fg-4 focus:outline-none"
              />
              <kbd className="rounded border border-line-strong px-1.5 font-mono text-2xs text-fg-4">esc</kbd>
            </div>
            <Command.List className="max-h-[420px] overflow-y-auto p-1.5 [&_[cmdk-group-heading]]:px-2.5 [&_[cmdk-group-heading]]:pb-1 [&_[cmdk-group-heading]]:pt-2.5 [&_[cmdk-group-heading]]:text-2xs [&_[cmdk-group-heading]]:font-medium [&_[cmdk-group-heading]]:uppercase [&_[cmdk-group-heading]]:tracking-[0.06em] [&_[cmdk-group-heading]]:text-fg-4">
              <Command.Empty className="px-3 py-8 text-center text-sm text-fg-3">{searching && (alerts.isFetching || cases.isFetching) ? "Searching…" : "No results"}</Command.Empty>
              {idMatch && (
                <Command.Group heading="Open by id">
                  <Item value={`open ${q}`} onSelect={() => go(idMatch[1](q.trim()))} icon={<ArrowRight />}>
                    Open {idMatch[2]} <span className="font-mono text-fg-3">{q.trim()}</span>
                  </Item>
                </Command.Group>
              )}
              {searching && (alerts.data?.items.length ?? 0) > 0 && (
                <Command.Group heading="Alerts">
                  {alerts.data!.items.map((a) => (
                    <Item key={a.id} value={`alert ${a.id} ${a.title}`} onSelect={() => go(`/alerts?alert=${a.id}`)} icon={<SeverityGlyph severity={a.severity} />}>
                      <span className="truncate">{a.title}</span>
                      <span className="ml-auto shrink-0 text-xs text-fg-4">{a.source_id}</span>
                    </Item>
                  ))}
                </Command.Group>
              )}
              {searching && (cases.data?.items.length ?? 0) > 0 && (
                <Command.Group heading="Cases">
                  {cases.data!.items.map((c) => (
                    <Item key={c.id} value={`case ${c.id} ${c.title}`} onSelect={() => go(`/cases/${c.id}`)} icon={<Briefcase />}>
                      <span className="truncate">{c.title}</span>
                      <span className="ml-auto shrink-0"><StatusBadge status={c.status} /></span>
                    </Item>
                  ))}
                </Command.Group>
              )}
              {!searching && (
                <>
                  <Command.Group heading="Go to">
                    {NAV.filter((n) => canAny(...n.perm)).map((n) => (
                      <Item key={n.to} value={`go ${n.label}`} onSelect={() => go(n.to)} icon={<n.icon />} hint={`g ${n.key}`}>
                        {n.label}
                      </Item>
                    ))}
                  </Command.Group>
                  <Command.Group heading="Quick filters">
                    {can("alerts:read") && (
                      <>
                        <Item value="critical high open alerts" onSelect={() => go("/alerts?status=new,acknowledged,in_progress&severity=5,4")} icon={<Bell />}>Open critical and high alerts</Item>
                        <Item value="my alerts assigned to me" onSelect={() => go("/alerts?assignee=me")} icon={<Bell />}>Alerts assigned to me</Item>
                      </>
                    )}
                    {can("cases:read") && <Item value="my cases" onSelect={() => go("/cases?assignee=me")} icon={<Briefcase />}>My cases</Item>}
                  </Command.Group>
                  <Command.Group heading="Preferences">
                    <Item value="toggle theme dark light" onSelect={() => run(() => prefs.set({ theme: document.documentElement.classList.contains("dark") ? "light" : "dark" }))} icon={document.documentElement.classList.contains("dark") ? <Sun /> : <Moon />}>
                      Switch to {document.documentElement.classList.contains("dark") ? "light" : "dark"} theme
                    </Item>
                    <Item value="toggle utc local time zone" onSelect={() => run(() => prefs.set({ utc: !prefs.utc }))} icon={<Clock />}>
                      Show times in {prefs.utc ? "local time" : "UTC"}
                    </Item>
                    <Item value="toggle density compact comfortable" onSelect={() => run(() => prefs.set({ density: prefs.density === "compact" ? "comfortable" : "compact" }))} icon={<Rows3 />}>
                      {prefs.density === "compact" ? "Comfortable" : "Compact"} rows
                    </Item>
                    <Item value="keyboard shortcuts help" onSelect={() => run(onShortcuts)} icon={<Keyboard />} hint="?">Keyboard shortcuts</Item>
                    <Item value="sign out log out" onSelect={() => run(() => void signOut())} icon={<LogOut />}>Sign out</Item>
                  </Command.Group>
                </>
              )}
            </Command.List>
          </Command>
        </D.Content>
      </D.Portal>
    </D.Root>
  );
}

function Item({ children, onSelect, icon, value, hint }: { children: React.ReactNode; onSelect: () => void; icon?: React.ReactNode; value: string; hint?: string }) {
  return (
    <Command.Item
      value={value}
      onSelect={onSelect}
      className="flex h-9 cursor-default select-none items-center gap-2.5 rounded-md px-2.5 text-sm text-fg-2 data-[selected=true]:bg-hover data-[selected=true]:text-fg [&_svg]:size-4 [&_svg]:shrink-0 [&_svg]:text-fg-3"
    >
      {icon}
      <span className="flex min-w-0 flex-1 items-center gap-2">{children}</span>
      {hint && <span className="font-mono text-2xs text-fg-4">{hint}</span>}
    </Command.Item>
  );
}
