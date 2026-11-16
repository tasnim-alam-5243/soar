import { useEffect } from "react";
import { useQueryClient, type QueryClient } from "@tanstack/react-query";
import { useNavigate } from "react-router-dom";
import { create } from "zustand";
import { toast } from "sonner";
import { config } from "./config";
import { getToken } from "./auth";
import { api, unwrap } from "./api";
import { usePrefs } from "./prefs";
import { sevName } from "./severity";

// Live updates over GET /stream (server-sent events). Each event invalidates the queries of
// the resource that changed (batched, so a burst of alerts costs one refetch), marks the row
// so lists can flash it, and raises a toast for new critical/high alerts and for approvals the
// viewer can decide.

interface UIEvent {
  topic: string;
  id: string;
  data?: Record<string, unknown>;
  at?: string;
}

interface LiveState {
  status: "connecting" | "live" | "offline";
  changed: Record<string, number>; // resource id -> when it last changed (ms)
  newAlerts: number; // critical/high alerts announced since the Alerts page was last opened
  set: (p: Partial<LiveState>) => void;
  bump: (id: string) => void;
}

export const useLive = create<LiveState>((set) => ({
  status: "connecting",
  changed: {},
  newAlerts: 0,
  set: (p) => set(p),
  bump: (id) => set((s) => ({ changed: { ...s.changed, [id]: Date.now() } })),
}));

/** useFlash returns a key that changes whenever the resource changed recently (for a flash animation). */
export function useFlash(id?: string): number | undefined {
  const at = useLive((s) => (id ? s.changed[id] : undefined));
  return at && Date.now() - at < 4000 ? at : undefined;
}

const related: Record<string, (id: string) => unknown[][]> = {
  alerts: (id) => [["alerts"], ["alert", id], ["dashboard"]],
  cases: (id) => [["cases"], ["case", id], ["timeline", id], ["case-alerts", id], ["dashboard"]],
  executions: (id) => [["executions"], ["execution", id], ["steps", id], ["dashboard"]],
  approvals: () => [["approvals"], ["dashboard"]],
};

function batcher(qc: QueryClient) {
  const pending = new Map<string, unknown[]>();
  let timer: ReturnType<typeof setTimeout> | undefined;
  return (keys: unknown[][]) => {
    for (const k of keys) pending.set(JSON.stringify(k), k);
    if (timer) return;
    timer = setTimeout(() => {
      timer = undefined;
      const all = [...pending.values()];
      pending.clear();
      for (const k of all) qc.invalidateQueries({ queryKey: k as unknown[] });
    }, 600);
  };
}

export function useLiveUpdates() {
  const qc = useQueryClient();
  const nav = useNavigate();
  useEffect(() => {
    const token = getToken();
    const url = `${config().apiBase}/stream${token ? `?access_token=${encodeURIComponent(token)}` : ""}`;
    const es = new EventSource(url);
    const invalidate = batcher(qc);
    const live = useLive.getState();
    es.onopen = () => live.set({ status: "live" });
    es.onerror = () => live.set({ status: es.readyState === EventSource.CLOSED ? "offline" : "connecting" });

    const onEvent = (topic: string) => (e: MessageEvent) => {
      let ev: UIEvent;
      try {
        ev = JSON.parse(e.data) as UIEvent;
      } catch {
        return;
      }
      invalidate(related[topic]?.(ev.id) ?? [[topic]]);
      if (ev.id) useLive.getState().bump(ev.id);
      if (topic === "alerts") onAlert(ev);
      if (topic === "approvals" && ev.data?.status === "pending") void onApproval(ev.id);
    };

    const onAlert = (ev: UIEvent) => {
      const created = Number(ev.data?.created ?? 0);
      const sev = Number(ev.data?.severity ?? 0);
      if (created && sev >= 4) {
        useLive.setState((s) => ({ newAlerts: s.newAlerts + 1 }));
        if (usePrefs.getState().toasts && !location.pathname.startsWith("/alerts")) {
          toast(`${sevName[sev]} alert`, {
            id: `alert-${ev.id}`,
            description: String(ev.data?.title ?? ev.id),
            action: { label: "View", onClick: () => nav(`/alerts?alert=${encodeURIComponent(ev.id)}`) },
            className: sev === 5 ? "toast-critical" : "toast-high",
          });
        }
      } else if (created > 1) {
        // a burst: one summary, never a toast storm
        if (usePrefs.getState().toasts && !location.pathname.startsWith("/alerts")) {
          toast(`${created} new alerts`, { id: "alert-burst", action: { label: "View", onClick: () => nav("/alerts") } });
        }
      }
    };

    const onApproval = async (id: string) => {
      if (!usePrefs.getState().toasts) return;
      try {
        const [a, me] = await Promise.all([
          unwrap(api().GET("/approvals/{id}", { params: { path: { id } } })),
          qc.fetchQuery({ queryKey: ["me"], queryFn: () => unwrap(api().GET("/me")), staleTime: 5 * 60_000 }),
        ]);
        const mine = a.approvers.some((x) => x === me.name || (me.roles ?? []).includes(x as never));
        if (!mine || !(me.permissions ?? []).includes("approvals:decide")) return;
        toast("Approval needed", {
          id: `approval-${id}`,
          description: `${a.workflow_name ?? "A workflow"} waits at ${a.node_id}`,
          action: { label: "Review", onClick: () => nav("/approvals") },
          duration: 12_000,
        });
      } catch {
        /* best effort */
      }
    };

    for (const topic of Object.keys(related)) es.addEventListener(topic, onEvent(topic) as EventListener);
    return () => {
      es.close();
      useLive.getState().set({ status: "offline" });
    };
  }, [qc, nav]);
}
