import * as React from "react";
import { ChevronDown } from "lucide-react";
import { cn } from "@/lib/utils";

const field =
  "w-full rounded-md border border-line-strong bg-panel px-2.5 text-sm text-fg placeholder:text-fg-4 transition-colors hover:border-fg-4/60 focus:border-accent focus:outline-none focus:ring-2 focus:ring-accent/20 disabled:cursor-not-allowed disabled:opacity-50";

export const Input = React.forwardRef<HTMLInputElement, React.InputHTMLAttributes<HTMLInputElement> & { icon?: React.ReactNode; trailing?: React.ReactNode }>(
  ({ className, icon, trailing, ...p }, ref) => {
    if (!icon && !trailing) return <input ref={ref} className={cn(field, "h-8", className)} {...p} />;
    return (
      <div className={cn("relative flex items-center", className)}>
        {icon && <span className="pointer-events-none absolute left-2.5 text-fg-3 [&_svg]:size-4">{icon}</span>}
        <input ref={ref} className={cn(field, "h-8", icon && "pl-8", trailing && "pr-14")} {...p} />
        {trailing && <span className="absolute right-2 flex items-center">{trailing}</span>}
      </div>
    );
  },
);
Input.displayName = "Input";

export const Textarea = React.forwardRef<HTMLTextAreaElement, React.TextareaHTMLAttributes<HTMLTextAreaElement>>(({ className, ...p }, ref) => (
  <textarea ref={ref} className={cn(field, "min-h-[88px] py-2 font-mono text-xs leading-5", className)} {...p} />
));
Textarea.displayName = "Textarea";

export const Label = ({ className, ...p }: React.LabelHTMLAttributes<HTMLLabelElement>) => (
  <label className={cn("text-xs font-medium text-fg-2", className)} {...p} />
);

/** Select is a styled native select: fast, accessible, keyboard-friendly. */
export const Select = React.forwardRef<HTMLSelectElement, React.SelectHTMLAttributes<HTMLSelectElement>>(({ className, children, ...p }, ref) => (
  <div className={cn("relative", className)}>
    <select ref={ref} className={cn(field, "h-8 appearance-none pr-8")} {...p}>
      {children}
    </select>
    <ChevronDown className="pointer-events-none absolute right-2 top-1/2 size-4 -translate-y-1/2 text-fg-3" />
  </div>
));
Select.displayName = "Select";

/** FormField stacks a label, a control and an optional hint or error. */
export function FormField({ label, hint, error, children, className }: { label: string; hint?: React.ReactNode; error?: string; children: React.ReactNode; className?: string }) {
  return (
    <div className={cn("flex flex-col gap-1.5", className)}>
      <Label>{label}</Label>
      {children}
      {error ? <span className="text-xs text-crit">{error}</span> : hint ? <span className="text-xs text-fg-3">{hint}</span> : null}
    </div>
  );
}

export function Checkbox({ checked, indeterminate, onChange, className, ...p }: {
  checked: boolean;
  indeterminate?: boolean;
  onChange: (v: boolean) => void;
  className?: string;
} & Omit<React.InputHTMLAttributes<HTMLInputElement>, "onChange" | "checked">) {
  const ref = React.useRef<HTMLInputElement>(null);
  React.useEffect(() => {
    if (ref.current) ref.current.indeterminate = !!indeterminate;
  }, [indeterminate]);
  return (
    <input
      ref={ref}
      type="checkbox"
      checked={checked}
      onChange={(e) => onChange(e.target.checked)}
      onClick={(e) => e.stopPropagation()}
      className={cn("size-3.5 cursor-pointer rounded-[3px] border-line-strong accent-[rgb(var(--accent))]", className)}
      {...p}
    />
  );
}

export function Switch({ checked, onChange, label, disabled }: { checked: boolean; onChange: (v: boolean) => void; label?: string; disabled?: boolean }) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={cn(
        "relative inline-flex h-[18px] w-8 shrink-0 items-center rounded-full border transition-colors disabled:opacity-50",
        checked ? "border-accent bg-accent" : "border-line-strong bg-hover",
      )}
    >
      <span className={cn("block size-3 rounded-full bg-white shadow transition-transform", checked ? "translate-x-[15px]" : "translate-x-[2px]")} />
    </button>
  );
}

/** Segmented is a compact single-choice control (ranges, status filters). */
export function Segmented<T extends string>({ value, onChange, options, size = "sm", className }: {
  value: T;
  onChange: (v: T) => void;
  options: { value: T; label: React.ReactNode; count?: number }[];
  size?: "xs" | "sm";
  className?: string;
}) {
  return (
    <div role="radiogroup" className={cn("inline-flex items-center gap-0.5 rounded-md border border-line bg-panel p-0.5", className)}>
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          role="radio"
          aria-checked={value === o.value}
          onClick={() => onChange(o.value)}
          className={cn(
            "inline-flex items-center gap-1.5 rounded-[5px] px-2 font-medium transition-colors",
            size === "xs" ? "h-5 text-2xs" : "h-6 text-xs",
            value === o.value ? "bg-hover text-fg shadow-[0_0_0_1px_rgb(var(--line-strong))]" : "text-fg-3 hover:text-fg",
          )}
        >
          {o.label}
          {o.count !== undefined && <span className="tabular text-fg-4">{o.count}</span>}
        </button>
      ))}
    </div>
  );
}

export const Kbd = ({ children, className }: { children: React.ReactNode; className?: string }) => (
  <kbd className={cn("inline-flex h-5 min-w-5 items-center justify-center rounded border border-line-strong bg-raised px-1 font-mono text-[10px] text-fg-3", className)}>
    {children}
  </kbd>
);
