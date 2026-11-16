import * as React from "react";
import * as M from "@radix-ui/react-dropdown-menu";
import * as P from "@radix-ui/react-popover";
import { Check } from "lucide-react";
import { cn } from "@/lib/utils";

const surface = "z-[60] min-w-[180px] overflow-hidden rounded-lg border border-line-strong bg-raised p-1 text-sm shadow-pop animate-pop-in";

export const Menu = M.Root;
export const MenuTrigger = M.Trigger;

export const MenuContent = ({ className, align = "end", ...p }: React.ComponentProps<typeof M.Content>) => (
  <M.Portal>
    <M.Content align={align} sideOffset={6} collisionPadding={8} className={cn(surface, className)} {...p} />
  </M.Portal>
);

export const MenuItem = ({ className, icon, shortcut, danger, children, ...p }: React.ComponentProps<typeof M.Item> & { icon?: React.ReactNode; shortcut?: string; danger?: boolean }) => (
  <M.Item
    className={cn(
      "flex h-8 cursor-default select-none items-center gap-2 rounded-md px-2 outline-none data-[disabled]:pointer-events-none data-[highlighted]:bg-hover data-[disabled]:opacity-45 [&_svg]:size-4 [&_svg]:text-fg-3",
      danger && "text-crit [&_svg]:text-crit",
      className,
    )}
    {...p}
  >
    {icon}
    <span className="flex-1 truncate">{children}</span>
    {shortcut && <span className="font-mono text-2xs text-fg-4">{shortcut}</span>}
  </M.Item>
);

export const MenuCheck = ({ checked, children, ...p }: React.ComponentProps<typeof M.CheckboxItem>) => (
  <M.CheckboxItem
    checked={checked}
    className="flex h-8 cursor-default select-none items-center gap-2 rounded-md px-2 outline-none data-[highlighted]:bg-hover"
    {...p}
  >
    <span className="flex size-4 items-center justify-center">{checked && <Check className="size-3.5 text-accent" />}</span>
    {children}
  </M.CheckboxItem>
);

export const MenuLabel = ({ className, ...p }: React.ComponentProps<typeof M.Label>) => (
  <M.Label className={cn("px-2 pb-1 pt-2 text-2xs font-medium uppercase tracking-[0.06em] text-fg-4", className)} {...p} />
);

export const MenuSeparator = () => <M.Separator className="-mx-1 my-1 h-px bg-line" />;

export const MenuSub = M.Sub;
export const MenuSubTrigger = ({ className, icon, children, ...p }: React.ComponentProps<typeof M.SubTrigger> & { icon?: React.ReactNode }) => (
  <M.SubTrigger className={cn("flex h-8 cursor-default select-none items-center gap-2 rounded-md px-2 outline-none data-[highlighted]:bg-hover data-[state=open]:bg-hover [&_svg]:size-4 [&_svg]:text-fg-3", className)} {...p}>
    {icon}
    <span className="flex-1">{children}</span>
    <span className="text-fg-4">›</span>
  </M.SubTrigger>
);
export const MenuSubContent = ({ className, ...p }: React.ComponentProps<typeof M.SubContent>) => (
  <M.Portal>
    <M.SubContent sideOffset={6} collisionPadding={8} className={cn(surface, className)} {...p} />
  </M.Portal>
);

export const Popover = P.Root;
export const PopoverTrigger = P.Trigger;
export const PopoverContent = ({ className, align = "start", ...p }: React.ComponentProps<typeof P.Content>) => (
  <P.Portal>
    <P.Content align={align} sideOffset={6} collisionPadding={8} className={cn(surface, "p-0", className)} {...p} />
  </P.Portal>
);
