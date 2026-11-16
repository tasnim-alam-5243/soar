import * as React from "react";
import { cn } from "@/lib/utils";

// Plain tables for short lists (admin, settings). Long lists use DataTable (virtualized).

export const Table = ({ className, ...p }: React.TableHTMLAttributes<HTMLTableElement>) => (
  <div className="w-full overflow-x-auto">
    <table className={cn("w-full border-collapse text-sm", className)} {...p} />
  </div>
);
export const THead = (p: React.HTMLAttributes<HTMLTableSectionElement>) => <thead className="sticky top-0 z-[1] bg-panel" {...p} />;
export const TBody = (p: React.HTMLAttributes<HTMLTableSectionElement>) => <tbody {...p} />;
export const TR = ({ className, ...p }: React.HTMLAttributes<HTMLTableRowElement>) => (
  <tr className={cn("border-b border-line last:border-0 [tbody_&]:hover:bg-hover/60", className)} {...p} />
);
export const TH = ({ className, ...p }: React.ThHTMLAttributes<HTMLTableCellElement>) => (
  <th className={cn("h-9 whitespace-nowrap border-b border-line px-3 text-left align-middle text-2xs font-medium uppercase tracking-[0.05em] text-fg-3", className)} {...p} />
);
export const TD = ({ className, ...p }: React.TdHTMLAttributes<HTMLTableCellElement>) => (
  <td className={cn("h-[var(--row-h)] px-3 align-middle", className)} {...p} />
);
