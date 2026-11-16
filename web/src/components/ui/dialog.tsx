import * as React from "react";
import * as D from "@radix-ui/react-dialog";
import { X } from "lucide-react";
import { cn } from "@/lib/utils";

export const Dialog = D.Root;

export function DialogContent({ title, description, className, children, footer, wide }: {
  title: React.ReactNode;
  description?: React.ReactNode;
  className?: string;
  children: React.ReactNode;
  footer?: React.ReactNode;
  wide?: boolean;
}) {
  return (
    <D.Portal>
      <D.Overlay className="fixed inset-0 z-50 bg-black/50 backdrop-blur-[2px] animate-fade-in" />
      <D.Content
        className={cn(
          "fixed left-1/2 top-[12vh] z-50 flex max-h-[80vh] w-[calc(100vw-32px)] -translate-x-1/2 flex-col rounded-xl border border-line-strong bg-raised shadow-pop animate-pop-in focus:outline-none",
          wide ? "max-w-3xl" : "max-w-lg",
          className,
        )}
      >
        <div className="flex items-start gap-3 border-b border-line px-5 py-4">
          <div className="min-w-0 flex-1">
            <D.Title className="text-[15px] font-semibold leading-6">{title}</D.Title>
            {description ? <D.Description className="mt-0.5 text-sm text-fg-3">{description}</D.Description> : <D.Description className="sr-only">{typeof title === "string" ? title : "dialog"}</D.Description>}
          </div>
          <D.Close className="rounded-md p-1 text-fg-3 hover:bg-hover hover:text-fg" aria-label="Close">
            <X className="size-4" />
          </D.Close>
        </div>
        <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-5 py-4">{children}</div>
        {footer && <div className="flex items-center justify-end gap-2 border-t border-line px-5 py-3">{footer}</div>}
      </D.Content>
    </D.Portal>
  );
}

/** Sheet is a panel that slides in from the right (detail views). */
export function Sheet({ open, onOpenChange, children, title, width = 560 }: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  children: React.ReactNode;
  title: string;
  width?: number;
}) {
  return (
    <D.Root open={open} onOpenChange={onOpenChange}>
      <D.Portal>
        <D.Overlay className="fixed inset-0 z-40 bg-black/30 animate-fade-in" />
        <D.Content
          style={{ width: `min(${width}px, 100vw)` }}
          className="fixed bottom-0 right-0 top-0 z-50 flex flex-col border-l border-line-strong bg-panel shadow-pop animate-slide-in focus:outline-none"
        >
          <D.Title className="sr-only">{title}</D.Title>
          <D.Description className="sr-only">{title}</D.Description>
          {children}
        </D.Content>
      </D.Portal>
    </D.Root>
  );
}

/** confirmable is a small promise-based confirmation dialog host. */
type ConfirmReq = { title: string; body?: React.ReactNode; confirm?: string; danger?: boolean; resolve: (ok: boolean) => void };
let push: ((r: ConfirmReq) => void) | null = null;

export function confirmDialog(opts: Omit<ConfirmReq, "resolve">): Promise<boolean> {
  return new Promise((resolve) => (push ? push({ ...opts, resolve }) : resolve(window.confirm(opts.title))));
}

export function ConfirmHost() {
  const [req, setReq] = React.useState<ConfirmReq | null>(null);
  React.useEffect(() => {
    push = setReq;
    return () => {
      push = null;
    };
  }, []);
  const done = (ok: boolean) => {
    req?.resolve(ok);
    setReq(null);
  };
  return (
    <Dialog open={!!req} onOpenChange={(o) => !o && done(false)}>
      {req && (
        <DialogContent
          title={req.title}
          footer={
            <>
              <button className="h-8 rounded-md px-3 text-sm text-fg-2 hover:bg-hover" onClick={() => done(false)}>Cancel</button>
              <button
                autoFocus
                className={cn("h-8 rounded-md px-3 text-sm font-medium", req.danger ? "bg-crit text-white hover:bg-crit/90" : "bg-accent text-accent-fg hover:bg-accent/90")}
                onClick={() => done(true)}
              >
                {req.confirm ?? "Confirm"}
              </button>
            </>
          }
        >
          {req.body && <div className="text-sm text-fg-2">{req.body}</div>}
        </DialogContent>
      )}
    </Dialog>
  );
}
