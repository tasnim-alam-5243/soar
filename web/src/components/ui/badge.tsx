import * as React from "react";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "@/lib/utils";

const badge = cva(
  "inline-flex max-w-full items-center gap-1 whitespace-nowrap rounded-[5px] border px-1.5 text-2xs font-medium leading-[18px] [&_svg]:size-3 [&_svg]:shrink-0",
  {
    variants: {
      tone: {
        gray: "border-line-strong bg-hover text-fg-2",
        outline: "border-line-strong bg-transparent text-fg-2",
        accent: "border-accent/30 bg-accent/10 text-accent",
        blue: "border-accent/30 bg-accent/10 text-accent",
        green: "border-good/30 bg-good/10 text-good",
        yellow: "border-warn/30 bg-warn/10 text-warn",
        orange: "border-serious/30 bg-serious/10 text-serious",
        red: "border-crit/30 bg-crit/10 text-crit",
      },
    },
    defaultVariants: { tone: "gray" },
  },
);

export interface BadgeProps extends React.HTMLAttributes<HTMLSpanElement>, VariantProps<typeof badge> {}

export const Badge = ({ className, tone, ...p }: BadgeProps) => <span className={cn(badge({ tone }), className)} {...p} />;
