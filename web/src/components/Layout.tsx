import { useEffect, useState } from "react";
import { NavLink, Outlet, useLocation, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ChevronsLeft, ChevronsRight, Keyboard, LogOut, Moon, Search, SlidersHorizontal, Sun } from "lucide-react";
import { api, unwrap } from "@/lib/api";
import { signOut, useAuth } from "@/lib/auth";
import { modKey, useHotkeys } from "@/lib/hotkeys";
import { usePerms } from "@/lib/perms";
import { usePrefs } from "@/lib/prefs";
import { useLive, useLiveUpdates } from "@/lib/stream";
import { cn } from "@/lib/utils";
import { Avatar, LiveDot } from "./common";
import { CommandPalette } from "./CommandPalette";
import { Logo } from "./Logo";
import { NAV } from "./nav";
import { PreferencesDialog, ShortcutsDialog } from "./Preferences";
import { Menu, MenuContent, MenuItem, MenuLabel, MenuSeparator, MenuTrigger } from "./ui/menu";
import { Tip } from "./ui/tooltip";

function useUnread() {
  const { can } = usePerms();
  const since = usePrefs((s) => s.lastSeen.alerts);
  const alerts = useQuery({
    queryKey: ["alerts", "unread", since],
    enabled: can("alerts:read"),
    queryFn: () =>
      unwrap(api().GET("/alerts", { params: { query: { status: "new", min_severity: 4, limit: 100, ...(since ? { from: since.replace(/\.\d+Z$/, "Z") } : {}) } } })),
    refetchInterval: 60_000,
  });
  const approvals = useQuery({
    queryKey: ["approvals", "pending-count"],
    enabled: can("approvals:read"),
    queryFn: () => unwrap(api().GET("/approvals", { params: { query: { status: "pending", limit: 100 } } })),
    refetchInterval: 60_000,
  });
  return { alerts: alerts.data?.items.length ?? 0, approvals: approvals.data?.items.length ?? 0 };
}

export function Layout() {
  useLiveUpdates();
  const nav = useNavigate();
  const loc = useLocation();
  const live = useLive((s) => s.status);
  const name = useAuth((s) => s.name);
  const { me, canAny } = usePerms();
  const prefs = usePrefs();
  const unread = useUnread();
  const [palette, setPalette] = useState(false);
  const [shortcuts, setShortcuts] = useState(false);
  const [preferences, setPreferences] = useState(false);
  const collapsed = prefs.sidebarCollapsed;

  const items = NAV.filter((n) => canAny(...n.perm));
  const goKeys = Object.fromEntries(items.map((n) => [`g ${n.key}`, () => nav(n.to)]));
  useHotkeys({ "mod+k": () => setPalette((o) => !o), "?": () => setShortcuts(true), ...goKeys }, [items.length]);

  useEffect(() => {
    const sm = window.matchMedia("(max-width: 1023px)");
    if (sm.matches && !prefs.sidebarCollapsed) prefs.set({ sidebarCollapsed: true });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const dark = document.documentElement.classList.contains("dark");
  const badge = (to: string) => (to === "/alerts" ? unread.alerts : to === "/approvals" ? unread.approvals : 0);
  let lastGroup: string | undefined;

  return (
    <div className="flex h-full">
      <aside className={cn("flex shrink-0 flex-col border-r border-line bg-panel transition-[width] duration-150", collapsed ? "w-[52px]" : "w-[232px]")}>
        <div className={cn("flex h-12 items-center gap-2.5", collapsed ? "justify-center" : "px-3.5")}>
          <Logo />
          {!collapsed && <span className="text-[15px] font-semibold tracking-[-0.01em]">SOAR</span>}
          {!collapsed && <span className="ml-auto"><LiveDot status={live} /></span>}
        </div>
        <div className={cn("px-2 pb-2", collapsed && "flex justify-center")}>
          <Tip content={collapsed ? `Search (${modKey} K)` : undefined} side="right">
            <button
              onClick={() => setPalette(true)}
              className={cn(
                "flex h-8 items-center gap-2 rounded-md border border-line bg-bg/50 text-sm text-fg-3 transition-colors hover:border-line-strong hover:text-fg-2",
                collapsed ? "w-8 justify-center" : "w-full px-2.5",
              )}
              aria-label="Search and commands"
            >
              <Search className="size-4" />
              {!collapsed && (
                <>
                  <span className="flex-1 text-left">Search…</span>
                  <kbd className="font-mono text-2xs text-fg-4">{modKey}K</kbd>
                </>
              )}
            </button>
          </Tip>
        </div>
        <nav className="flex flex-1 flex-col gap-px overflow-y-auto px-2" aria-label="Main">
          {items.map((n) => {
            const group = "group" in n ? n.group : undefined;
            const header = group && group !== lastGroup && !collapsed ? group : null;
            lastGroup = group;
            const count = badge(n.to);
            return (
              <div key={n.to}>
                {header && <div className="label px-2.5 pb-1 pt-4">{header}</div>}
                {group && collapsed && group !== (items[items.indexOf(n) - 1] as { group?: string } | undefined)?.group && <div className="mx-2 my-2 h-px bg-line" />}
                <Tip content={collapsed ? n.label : undefined} side="right">
                  <NavLink
                    to={n.to}
                    end={"end" in n ? n.end : undefined}
                    className={({ isActive }) =>
                      cn(
                        "group relative flex h-8 items-center gap-2.5 rounded-md text-sm transition-colors",
                        collapsed ? "justify-center" : "px-2.5",
                        isActive ? "bg-hover font-medium text-fg" : "text-fg-2 hover:bg-hover/60 hover:text-fg",
                      )
                    }
                  >
                    {({ isActive }) => (
                      <>
                        {isActive && <span className="absolute -left-2 top-1.5 h-5 w-[3px] rounded-r bg-accent" />}
                        <n.icon className={cn("size-4 shrink-0", isActive ? "text-fg" : "text-fg-3 group-hover:text-fg-2")} />
                        {!collapsed && <span className="flex-1 truncate">{n.label}</span>}
                        {count > 0 && (
                          <span
                            className={cn(
                              "tabular rounded-full text-[10px] font-semibold leading-4",
                              collapsed ? "absolute right-0.5 top-0.5 min-w-4 bg-accent px-1 text-center text-accent-fg" : "min-w-5 bg-accent/15 px-1.5 text-center text-accent",
                            )}
                            aria-label={`${count} unread`}
                          >
                            {count > 99 ? "99+" : count}
                          </span>
                        )}
                      </>
                    )}
                  </NavLink>
                </Tip>
              </div>
            );
          })}
        </nav>
        <div className={cn("flex items-center gap-1 border-t border-line p-2", collapsed && "flex-col")}>
          <Menu>
            <MenuTrigger asChild>
              <button className={cn("flex min-w-0 items-center gap-2 rounded-md p-1.5 text-left hover:bg-hover", !collapsed && "flex-1")} aria-label="Account">
                <Avatar name={me?.name ?? name} size={24} />
                {!collapsed && (
                  <span className="min-w-0 flex-1">
                    <span className="block truncate text-sm font-medium leading-4">{me?.name ?? name ?? "Signed in"}</span>
                    <span className="block truncate text-2xs text-fg-3">{me?.roles?.join(", ") ?? ""}</span>
                  </span>
                )}
              </button>
            </MenuTrigger>
            <MenuContent align="start" side="top" className="w-56">
              <MenuLabel>{me?.type === "api_key" ? "Signed in with an API key" : "Signed in"}</MenuLabel>
              <MenuItem icon={<SlidersHorizontal />} onSelect={() => setPreferences(true)}>Preferences</MenuItem>
              <MenuItem icon={dark ? <Sun /> : <Moon />} onSelect={() => prefs.set({ theme: dark ? "light" : "dark" })}>{dark ? "Light" : "Dark"} theme</MenuItem>
              <MenuItem icon={<Keyboard />} shortcut="?" onSelect={() => setShortcuts(true)}>Keyboard shortcuts</MenuItem>
              <MenuSeparator />
              <MenuItem icon={<LogOut />} onSelect={() => void signOut()}>Sign out</MenuItem>
            </MenuContent>
          </Menu>
          <Tip content={collapsed ? "Expand sidebar" : "Collapse sidebar"} side="right">
            <button className="rounded-md p-1.5 text-fg-3 hover:bg-hover hover:text-fg" onClick={() => prefs.set({ sidebarCollapsed: !collapsed })} aria-label="Toggle sidebar">
              {collapsed ? <ChevronsRight className="size-4" /> : <ChevronsLeft className="size-4" />}
            </button>
          </Tip>
        </div>
      </aside>
      <main key={loc.pathname.split("/")[1]} className="min-w-0 flex-1 overflow-auto">
        <Outlet />
      </main>
      <CommandPalette open={palette} onOpenChange={setPalette} onShortcuts={() => setShortcuts(true)} />
      <ShortcutsDialog open={shortcuts} onOpenChange={setShortcuts} />
      <PreferencesDialog open={preferences} onOpenChange={setPreferences} />
    </div>
  );
}

/** Page is the padded content area every page renders into. */
export function Page({ children, className, wide }: { children: React.ReactNode; className?: string; wide?: boolean }) {
  return <div className={cn("mx-auto w-full px-6 py-6", wide ? "max-w-none" : "max-w-[1440px]", className)}>{children}</div>;
}
