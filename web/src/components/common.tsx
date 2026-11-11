import { AlertTriangle, Inbox, Loader2 } from "lucide-react";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";
import { Problem } from "@/lib/api";
import { pretty } from "@/lib/utils";

const statusTone: Record<string, "gray" | "blue" | "green" | "yellow" | "orange" | "red"> = {
  queued: "gray", running: "blue", waiting: "yellow", succeeded: "green", failed: "red", cancelled: "gray", timed_out: "orange",
  retry_wait: "yellow", skipped: "gray", pending: "yellow", approved: "green", rejected: "red", expired: "orange",
  open: "blue", in_progress: "yellow", closed: "green", done: "green", draft: "gray", published: "green", archived: "gray",
};

export function StatusBadge({ status }: { status?: string }) {
  return <Badge tone={statusTone[status ?? ""] ?? "gray"}>{status ?? "—"}</Badge>;
}

const sevNames = ["", "info", "low", "medium", "high", "critical"];
const sevTone: ("gray" | "blue" | "yellow" | "orange" | "red")[] = ["gray", "gray", "blue", "yellow", "orange", "red"];

export function SeverityBadge({ severity }: { severity?: number }) {
  const s = Math.min(5, Math.max(0, severity ?? 0));
  return <Badge tone={sevTone[s]}>{s ? `${s} · ${sevNames[s]}` : "—"}</Badge>;
}

export function PageHeader({ title, subtitle, actions }: { title: string; subtitle?: string; actions?: React.ReactNode }) {
  return (
    <div className="mb-4 flex flex-wrap items-end justify-between gap-3">
      <div>
        <h1 className="text-xl font-semibold">{title}</h1>
        {subtitle && <p className="text-sm text-muted-foreground">{subtitle}</p>}
      </div>
      <div className="flex gap-2">{actions}</div>
    </div>
  );
}

export function Loading({ label = "Loading" }: { label?: string }) {
  return (
    <div className="flex items-center gap-2 p-6 text-sm text-muted-foreground">
      <Loader2 className="h-4 w-4 animate-spin" /> {label}…
    </div>
  );
}

export function ErrorBox({ error }: { error: unknown }) {
  if (!error) return null;
  const p = error instanceof Problem ? error : null;
  return (
    <div className="rounded-md border border-destructive/40 bg-destructive/5 p-3 text-sm text-destructive">
      <div className="flex items-center gap-2 font-medium">
        <AlertTriangle className="h-4 w-4" /> {p ? p.title : String(error)}
      </div>
      {p?.detail && <p className="mt-1">{p.detail}</p>}
      {p?.errors && p.errors.length > 0 && (
        <ul className="mt-1 list-disc pl-5">
          {p.errors.map((e, i) => (
            <li key={i}>
              <code className="font-mono">{e.path}</code>: {e.message}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

export function Empty({ text }: { text: string }) {
  return (
    <div className="flex flex-col items-center gap-2 p-10 text-sm text-muted-foreground">
      <Inbox className="h-6 w-6" /> {text}
    </div>
  );
}

export function JsonView({ value, className }: { value: unknown; className?: string }) {
  return <pre className={"max-h-96 overflow-auto rounded-md bg-muted p-3 font-mono text-xs " + (className ?? "")}>{pretty(value)}</pre>;
}

export function Pager({ cursor, onNext, onFirst, hasPrev }: { cursor?: string; onNext: (c: string) => void; onFirst: () => void; hasPrev: boolean }) {
  return (
    <div className="flex justify-end gap-2 p-3">
      <Button variant="outline" size="sm" disabled={!hasPrev} onClick={onFirst}>
        First page
      </Button>
      <Button variant="outline" size="sm" disabled={!cursor} onClick={() => cursor && onNext(cursor)}>
        Next
      </Button>
    </div>
  );
}

export function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-1">
      <span className="text-xs font-medium text-muted-foreground">{label}</span>
      <div className="text-sm">{children}</div>
    </div>
  );
}
