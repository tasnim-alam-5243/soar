import { Bell, Briefcase, Cable, GitBranch, LayoutDashboard, Play, Radio, Settings2, ShieldCheck } from "lucide-react";

// The navigation, with the permission each page needs (pages the caller cannot read are hidden).
export const NAV = [
  { to: "/", label: "Overview", icon: LayoutDashboard, perm: ["alerts:read"], key: "d", end: true },
  { to: "/alerts", label: "Alerts", icon: Bell, perm: ["alerts:read"], key: "a" },
  { to: "/cases", label: "Cases", icon: Briefcase, perm: ["cases:read"], key: "c" },
  { to: "/approvals", label: "Approvals", icon: ShieldCheck, perm: ["approvals:read"], key: "p" },
  { to: "/workflows", label: "Workflows", icon: GitBranch, perm: ["workflows:read"], key: "w", group: "Automation" },
  { to: "/executions", label: "Executions", icon: Play, perm: ["executions:read"], key: "e", group: "Automation" },
  { to: "/sources", label: "Sources", icon: Radio, perm: ["sources:read"], key: "s", group: "Configuration" },
  { to: "/connections", label: "Connections", icon: Cable, perm: ["connections:read"], key: "o", group: "Configuration" },
  { to: "/admin", label: "Admin", icon: Settings2, perm: ["principals:read", "audit:read", "queue:read"], key: "m", group: "Configuration" },
] as const;

export type NavItem = (typeof NAV)[number];
