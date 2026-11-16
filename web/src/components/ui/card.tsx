import * as React from "react";
import { cn } from "@/lib/utils";

export const Card = ({ className, ...p }: React.HTMLAttributes<HTMLDivElement>) => (
  <div className={cn("rounded-lg border border-line bg-panel shadow-card", className)} {...p} />
);

/** Panel is a card with a title row (title, optional subtitle, actions on the right). */
export function Panel({ title, subtitle, actions, children, className, bodyClassName, icon }: {
  title?: React.ReactNode;
  subtitle?: React.ReactNode;
  actions?: React.ReactNode;
  icon?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
  bodyClassName?: string;
}) {
  return (
    <Card className={cn("flex min-w-0 flex-col", className)}>
      {(title || actions) && (
        <div className="flex min-h-11 items-center gap-2 border-b border-line px-4 py-2">
          {icon && <span className="text-fg-3 [&_svg]:size-4">{icon}</span>}
          <div className="min-w-0 flex-1">
            {title && <h3 className="truncate text-sm font-semibold">{title}</h3>}
            {subtitle && <p className="truncate text-xs text-fg-3">{subtitle}</p>}
          </div>
          {actions && <div className="flex shrink-0 items-center gap-1.5">{actions}</div>}
        </div>
      )}
      <div className={cn("min-h-0 flex-1 p-4", bodyClassName)}>{children}</div>
    </Card>
  );
}

// kept for pages that compose their own headers
export const CardHeader = ({ className, ...p }: React.HTMLAttributes<HTMLDivElement>) => (
  <div className={cn("flex items-center gap-2 border-b border-line px-4 py-3", className)} {...p} />
);
export const CardTitle = ({ className, ...p }: React.HTMLAttributes<HTMLHeadingElement>) => (
  <h3 className={cn("text-sm font-semibold", className)} {...p} />
);
export const CardContent = ({ className, ...p }: React.HTMLAttributes<HTMLDivElement>) => <div className={cn("p-4", className)} {...p} />;
