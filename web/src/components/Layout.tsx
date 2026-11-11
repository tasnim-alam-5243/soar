import { NavLink, Outlet } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Activity, Bell, Briefcase, Cable, GitBranch, LayoutDashboard, LogOut, Moon, Play, Radio, ShieldCheck, Sun } from "lucide-react";
import { api, unwrap } from "@/lib/api";
import { signOut, useAuth } from "@/lib/auth";
import { useLiveUpdates } from "@/lib/stream";
import { cn } from "@/lib/utils";

const nav = [
  { to: "/", label: "Dashboard", icon: LayoutDashboard, end: true },
  { to: "/alerts", label: "Alerts", icon: Bell },
  { to: "/cases", label: "Cases", icon: Briefcase },
  { to: "/workflows", label: "Workflows", icon: GitBranch },
  { to: "/executions", label: "Executions", icon: Play },
  { to: "/approvals", label: "Approvals", icon: ShieldCheck, badge: true },
  { to: "/sources", label: "Sources", icon: Radio },
  { to: "/connections", label: "Connections", icon: Cable },
  { to: "/admin", label: "Admin", icon: Activity },
];

export function useMe() {
  return useQuery({ queryKey: ["me"], queryFn: () => unwrap(api().GET("/me")), staleTime: 60_000 });
}

export const can = (perms: string[] | undefined, p: string) => !!perms?.includes(p);

export function Layout() {
  useLiveUpdates();
  const name = useAuth((s) => s.name);
  const me = useMe();
  const pending = useQuery({
    queryKey: ["approvals", "pending-count"],
    queryFn: () => unwrap(api().GET("/approvals", { params: { query: { status: "pending", limit: 50 } } })),
    refetchInterval: 30_000,
  });
  const nPending = pending.data?.items.length ?? 0;
  const toggleTheme = () => {
    const dark = document.documentElement.classList.toggle("dark");
    localStorage.setItem("soar.theme", dark ? "dark" : "light");
  };
  return (
    <div className="flex h-screen">
      <aside className="flex w-56 flex-col border-r bg-card">
        <div className="flex items-center gap-2 px-4 py-4 text-lg font-semibold">
          <ShieldCheck className="h-5 w-5 text-primary" /> SOAR
        </div>
        <nav className="flex flex-1 flex-col gap-0.5 px-2">
          {nav.map((n) => (
            <NavLink
              key={n.to}
              to={n.to}
              end={n.end}
              className={({ isActive }) => cn("flex items-center gap-2 rounded-md px-3 py-2 text-sm hover:bg-accent", isActive && "bg-accent font-medium")}
            >
              <n.icon className="h-4 w-4" />
              <span className="flex-1">{n.label}</span>
              {n.badge && nPending > 0 && <span className="rounded-full bg-amber-500 px-1.5 text-xs text-white">{nPending}</span>}
            </NavLink>
          ))}
        </nav>
        <div className="border-t p-3 text-xs text-muted-foreground">
          <div className="mb-2 truncate" title={me.data?.roles?.join(", ")}>
            {me.data?.name ?? name} {me.data?.roles && <span>· {me.data.roles.join(", ")}</span>}
          </div>
          <div className="flex gap-1">
            <button className="rounded p-1.5 hover:bg-accent" onClick={toggleTheme} aria-label="Toggle theme">
              <Sun className="h-4 w-4 dark:hidden" />
              <Moon className="hidden h-4 w-4 dark:block" />
            </button>
            <button className="flex items-center gap-1 rounded p-1.5 hover:bg-accent" onClick={() => signOut()}>
              <LogOut className="h-4 w-4" /> Sign out
            </button>
          </div>
        </div>
      </aside>
      <main className="flex-1 overflow-auto p-6">
        <Outlet />
      </main>
    </div>
  );
}
