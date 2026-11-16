import * as React from "react";
import * as T from "@radix-ui/react-tabs";
import { cn } from "@/lib/utils";

export const Tabs = T.Root;

export const TabsList = ({ className, ...p }: React.ComponentProps<typeof T.List>) => (
  <T.List className={cn("flex items-center gap-1 border-b border-line", className)} {...p} />
);

export const TabsTrigger = ({ className, children, count, ...p }: React.ComponentProps<typeof T.Trigger> & { count?: number }) => (
  <T.Trigger
    className={cn(
      "relative -mb-px inline-flex h-9 items-center gap-1.5 border-b-2 border-transparent px-2.5 text-sm font-medium text-fg-3 transition-colors hover:text-fg data-[state=active]:border-accent data-[state=active]:text-fg",
      className,
    )}
    {...p}
  >
    {children}
    {count !== undefined && <span className="tabular rounded bg-hover px-1 text-2xs text-fg-3">{count}</span>}
  </T.Trigger>
);

export const TabsContent = ({ className, ...p }: React.ComponentProps<typeof T.Content>) => (
  <T.Content className={cn("pt-4 focus:outline-none", className)} {...p} />
);
