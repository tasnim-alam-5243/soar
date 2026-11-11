import { useEffect } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { config } from "./config";
import { getToken } from "./auth";

// useLiveUpdates listens to GET /stream (server-sent events) and invalidates the
// queries of the changed resources, so open pages refresh on their own.
export function useLiveUpdates() {
  const qc = useQueryClient();
  useEffect(() => {
    const t = getToken();
    const url = `${config().apiBase}/stream${t ? `?access_token=${encodeURIComponent(t)}` : ""}`;
    const es = new EventSource(url);
    const on = (topic: string) => (e: MessageEvent) => {
      try {
        const ev = JSON.parse(e.data) as { id: string };
        qc.invalidateQueries({ queryKey: [topic] });
        qc.invalidateQueries({ queryKey: [topic.replace(/s$/, ""), ev.id] });
        if (topic === "executions") qc.invalidateQueries({ queryKey: ["dashboard"] });
      } catch {
        /* ignore malformed events */
      }
    };
    for (const topic of ["executions", "cases", "approvals", "alerts"]) es.addEventListener(topic, on(topic) as EventListener);
    return () => es.close();
  }, [qc]);
}
