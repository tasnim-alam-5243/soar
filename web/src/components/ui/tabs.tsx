import * as TabsPrimitive from "@radix-ui/react-tabs";
import { cn } from "@/lib/utils";

export const Tabs = TabsPrimitive.Root;
export const TabsList = ({ className, ...p }: TabsPrimitive.TabsListProps) => (
  <TabsPrimitive.List className={cn("inline-flex h-9 items-center rounded-md bg-muted p-1 text-muted-foreground", className)} {...p} />
);
export const TabsTrigger = ({ className, ...p }: TabsPrimitive.TabsTriggerProps) => (
  <TabsPrimitive.Trigger
    className={cn("inline-flex items-center rounded-sm px-3 py-1 text-sm font-medium data-[state=active]:bg-background data-[state=active]:text-foreground data-[state=active]:shadow-sm", className)}
    {...p}
  />
);
export const TabsContent = ({ className, ...p }: TabsPrimitive.TabsContentProps) => (
  <TabsPrimitive.Content className={cn("mt-3 focus-visible:outline-none", className)} {...p} />
);
