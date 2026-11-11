import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "@/lib/utils";

const badge = cva("inline-flex items-center rounded-full border px-2 py-0.5 text-xs font-medium", {
  variants: {
    tone: {
      gray: "border-transparent bg-secondary text-secondary-foreground",
      blue: "border-transparent bg-blue-100 text-blue-800 dark:bg-blue-900/40 dark:text-blue-200",
      green: "border-transparent bg-green-100 text-green-800 dark:bg-green-900/40 dark:text-green-200",
      yellow: "border-transparent bg-amber-100 text-amber-800 dark:bg-amber-900/40 dark:text-amber-200",
      orange: "border-transparent bg-orange-100 text-orange-800 dark:bg-orange-900/40 dark:text-orange-200",
      red: "border-transparent bg-red-100 text-red-800 dark:bg-red-900/40 dark:text-red-200",
      outline: "text-foreground",
    },
  },
  defaultVariants: { tone: "gray" },
});

export function Badge({ className, tone, ...p }: React.HTMLAttributes<HTMLSpanElement> & VariantProps<typeof badge>) {
  return <span className={cn(badge({ tone }), className)} {...p} />;
}
