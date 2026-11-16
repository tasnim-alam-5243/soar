import { Monitor, Moon, Sun } from "lucide-react";
import { SHORTCUTS } from "@/lib/hotkeys";
import { usePrefs, type Theme } from "@/lib/prefs";
import { Dialog, DialogContent } from "./ui/dialog";
import { Kbd, Segmented, Switch } from "./ui/input";

export function PreferencesDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (o: boolean) => void }) {
  const p = usePrefs();
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      {open && (
        <DialogContent title="Preferences" description="Saved in this browser only.">
          <Row label="Theme" hint="Dark suits dim SOC rooms; System follows your OS.">
            <Segmented<Theme>
              value={p.theme}
              onChange={(theme) => p.set({ theme })}
              options={[
                { value: "dark", label: <><Moon className="size-3.5" /> Dark</> },
                { value: "light", label: <><Sun className="size-3.5" /> Light</> },
                { value: "system", label: <><Monitor className="size-3.5" /> System</> },
              ]}
            />
          </Row>
          <Row label="Row density">
            <Segmented value={p.density} onChange={(density) => p.set({ density })} options={[{ value: "comfortable", label: "Comfortable" }, { value: "compact", label: "Compact" }]} />
          </Row>
          <Row label="Time zone" hint="Exact times show on hover everywhere.">
            <Segmented value={p.utc ? "utc" : "local"} onChange={(v) => p.set({ utc: v === "utc" })} options={[{ value: "local", label: "Local time" }, { value: "utc", label: "UTC" }]} />
          </Row>
          <Row label="Notifications" hint="Toasts for new critical and high alerts and for approvals you can decide.">
            <Switch checked={p.toasts} onChange={(toasts) => p.set({ toasts })} label="Notifications" />
          </Row>
        </DialogContent>
      )}
    </Dialog>
  );
}

function Row({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-4">
      <div>
        <div className="text-sm font-medium">{label}</div>
        {hint && <div className="text-xs text-fg-3">{hint}</div>}
      </div>
      {children}
    </div>
  );
}

export function ShortcutsDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (o: boolean) => void }) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      {open && (
        <DialogContent title="Keyboard shortcuts" wide>
          <div className="grid gap-6 sm:grid-cols-2">
            {SHORTCUTS.map((g) => (
              <div key={g.group} className="flex flex-col gap-1">
                <div className="label mb-1">{g.group}</div>
                {g.items.map(([k, what]) => (
                  <div key={k} className="flex items-center justify-between gap-3 py-1 text-sm">
                    <span className="text-fg-2">{what}</span>
                    <span className="flex gap-1">{k.split(" ").map((x, i) => (x === "/" && k.includes(" / ") ? <span key={i} className="text-fg-4">/</span> : <Kbd key={i}>{x}</Kbd>))}</span>
                  </div>
                ))}
              </div>
            ))}
          </div>
        </DialogContent>
      )}
    </Dialog>
  );
}
