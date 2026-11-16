import * as React from "react";
import { Slot } from "@radix-ui/react-slot";
import { cva, type VariantProps } from "class-variance-authority";
import { Loader2 } from "lucide-react";
import { cn } from "@/lib/utils";
import { Tip } from "./tooltip";

const button = cva(
  "relative inline-flex select-none items-center justify-center gap-1.5 whitespace-nowrap rounded-md font-medium transition-[background-color,color,box-shadow,opacity] duration-100 disabled:pointer-events-none disabled:opacity-45 [&_svg]:size-4 [&_svg]:shrink-0",
  {
    variants: {
      variant: {
        primary: "bg-accent text-accent-fg shadow-[inset_0_1px_0_rgb(255_255_255/0.15)] hover:bg-accent/90",
        secondary: "border border-line-strong bg-raised text-fg hover:bg-hover",
        outline: "border border-line-strong bg-transparent text-fg hover:bg-hover",
        ghost: "text-fg-2 hover:bg-hover hover:text-fg",
        danger: "bg-crit text-white hover:bg-crit/90",
        "danger-ghost": "text-crit hover:bg-crit/10",
        link: "h-auto px-0 text-accent underline-offset-4 hover:underline",
      },
      size: {
        xs: "h-6 px-2 text-xs [&_svg]:size-3.5",
        sm: "h-7 px-2.5 text-xs",
        md: "h-8 px-3 text-sm",
        lg: "h-9 px-4 text-sm",
        icon: "size-8",
        "icon-sm": "size-7 [&_svg]:size-3.5",
        "icon-xs": "size-6 [&_svg]:size-3.5",
      },
    },
    defaultVariants: { variant: "secondary", size: "md" },
  },
);

export interface ButtonProps extends React.ButtonHTMLAttributes<HTMLButtonElement>, VariantProps<typeof button> {
  asChild?: boolean;
  loading?: boolean;
  /** tooltip; also explains why a disabled button is disabled */
  tip?: React.ReactNode;
  kbd?: string;
}

export const Button = React.forwardRef<HTMLButtonElement, ButtonProps>(
  ({ className, variant, size, asChild, loading, tip, kbd, children, disabled, ...p }, ref) => {
    const Comp = asChild ? Slot : "button";
    const el = (
      <Comp ref={ref} className={cn(button({ variant, size }), className)} disabled={disabled || loading} {...p}>
        {asChild ? children : (
          <>
            {loading && <Loader2 className="animate-spin" />}
            {children}
            {kbd && <kbd className="ml-1 rounded border border-current/20 px-1 font-mono text-[10px] leading-4 opacity-60">{kbd}</kbd>}
          </>
        )}
      </Comp>
    );
    if (!tip) return el;
    // disabled buttons swallow pointer events: the wrapper carries the tooltip instead
    return <Tip content={tip}>{disabled ? <span className="inline-flex" tabIndex={0}>{el}</span> : el}</Tip>;
  },
);
Button.displayName = "Button";
