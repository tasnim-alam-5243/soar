import { create } from "zustand";
import { persist, createJSONStorage, type StateStorage } from "zustand/middleware";

// Per-browser preferences (spec: theme, density, columns, saved filters, time display,
// notifications). They live in localStorage only; nothing here is shared or sent anywhere.

export type Theme = "dark" | "light" | "system";
export type Density = "comfortable" | "compact";

export interface SavedView {
  name: string;
  params: Record<string, string>;
}

interface Prefs {
  theme: Theme;
  density: Density;
  utc: boolean;
  toasts: boolean;
  sidebarCollapsed: boolean;
  alertColumns: Record<string, boolean>;
  savedViews: Record<string, SavedView[]>; // page -> views
  lastSeen: Record<string, string>; // page -> ISO time of the last visit (unread badges)
  set: (p: Partial<Omit<Prefs, "set" | "saveView" | "deleteView" | "markSeen">>) => void;
  saveView: (page: string, v: SavedView) => void;
  deleteView: (page: string, name: string) => void;
  markSeen: (page: string) => void;
}

// localStorage can be missing or throw (private windows, blocked storage): fall back to memory.
const mem = new Map<string, string>();
const safeStorage: StateStorage = {
  getItem: (k) => {
    try {
      return localStorage.getItem(k);
    } catch {
      return mem.get(k) ?? null;
    }
  },
  setItem: (k, v) => {
    try {
      localStorage.setItem(k, v);
    } catch {
      mem.set(k, v);
    }
  },
  removeItem: (k) => {
    try {
      localStorage.removeItem(k);
    } catch {
      mem.delete(k);
    }
  },
};

export const usePrefs = create<Prefs>()(
  persist(
    (set) => ({
      theme: "dark",
      density: "comfortable",
      utc: false,
      toasts: true,
      sidebarCollapsed: false,
      alertColumns: { source: true, host: true, count: true, assignee: true, status: true, seen: true },
      savedViews: {},
      lastSeen: {},
      set: (p) => set(p),
      saveView: (page, v) =>
        set((s) => ({ savedViews: { ...s.savedViews, [page]: [...(s.savedViews[page] ?? []).filter((x) => x.name !== v.name), v] } })),
      deleteView: (page, name) =>
        set((s) => ({ savedViews: { ...s.savedViews, [page]: (s.savedViews[page] ?? []).filter((x) => x.name !== name) } })),
      markSeen: (page) => set((s) => ({ lastSeen: { ...s.lastSeen, [page]: new Date().toISOString() } })),
    }),
    { name: "soar.prefs", storage: createJSONStorage(() => safeStorage), version: 1 },
  ),
);

/** applyAppearance mirrors theme and density onto <html> (class "dark", data-density). */
export function applyAppearance(theme: Theme, density: Density) {
  const root = document.documentElement;
  const dark = theme === "dark" || (theme === "system" && window.matchMedia?.("(prefers-color-scheme: dark)").matches);
  root.classList.toggle("dark", !!dark);
  root.dataset.density = density;
}

export const isDark = () => document.documentElement.classList.contains("dark");
