/** @type {import('tailwindcss').Config} */
// Every color is a CSS custom property (src/index.css) so light and dark are two token sets,
// not two stylesheets. Channels are "r g b" so Tailwind opacity modifiers keep working.
const c = (v) => `rgb(var(--${v}) / <alpha-value>)`;

export default {
  darkMode: "class",
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        bg: c("bg"),
        panel: c("panel"),
        raised: c("raised"),
        hover: c("hover"),
        line: c("line"),
        "line-strong": c("line-strong"),
        fg: { DEFAULT: c("fg"), 2: c("fg-2"), 3: c("fg-3"), 4: c("fg-4") },
        accent: { DEFAULT: c("accent"), fg: c("accent-fg"), soft: c("accent-soft") },
        good: c("good"),
        warn: c("warn"),
        serious: c("serious"),
        crit: c("crit"),
        sev: { 1: c("sev-1"), 2: c("sev-2"), 3: c("sev-3"), 4: c("sev-4"), 5: c("sev-5") },
        // aliases used by the shared primitives
        border: c("line"),
        ring: c("accent"),
        background: c("bg"),
        foreground: c("fg"),
        muted: { DEFAULT: c("hover"), foreground: c("fg-3") },
        destructive: { DEFAULT: c("crit"), foreground: "255 255 255" },
      },
      fontFamily: {
        sans: ["'Inter Variable'", "ui-sans-serif", "system-ui", "-apple-system", "'Segoe UI'", "sans-serif"],
        mono: ["'JetBrains Mono Variable'", "ui-monospace", "SFMono-Regular", "Menlo", "monospace"],
      },
      fontSize: {
        "2xs": ["0.6875rem", { lineHeight: "1rem" }],
        xs: ["0.75rem", { lineHeight: "1.05rem" }],
        sm: ["0.8125rem", { lineHeight: "1.25rem" }],
        base: ["0.875rem", { lineHeight: "1.4rem" }],
      },
      borderRadius: { sm: "4px", DEFAULT: "6px", md: "6px", lg: "8px", xl: "12px" },
      boxShadow: {
        pop: "0 0 0 1px rgb(var(--line-strong)), 0 12px 32px -8px rgb(0 0 0 / 0.45), 0 4px 12px -4px rgb(0 0 0 / 0.25)",
        card: "0 1px 0 0 rgb(255 255 255 / 0.02) inset",
      },
      keyframes: {
        flash: { "0%": { backgroundColor: "rgb(var(--accent) / 0.18)" }, "100%": { backgroundColor: "rgb(var(--accent) / 0)" } },
        "fade-in": { from: { opacity: "0" }, to: { opacity: "1" } },
        "slide-in": { from: { transform: "translateX(12px)", opacity: "0" }, to: { transform: "translateX(0)", opacity: "1" } },
        "pop-in": { from: { transform: "scale(0.97)", opacity: "0" }, to: { transform: "scale(1)", opacity: "1" } },
        pulse2: { "0%,100%": { opacity: "1" }, "50%": { opacity: "0.35" } },
        shimmer: { "100%": { transform: "translateX(100%)" } },
      },
      animation: {
        flash: "flash 2.2s ease-out forwards",
        "fade-in": "fade-in 120ms ease-out",
        "slide-in": "slide-in 160ms cubic-bezier(0.2, 0.8, 0.2, 1)",
        "pop-in": "pop-in 120ms cubic-bezier(0.2, 0.8, 0.2, 1)",
        live: "pulse2 2s ease-in-out infinite",
      },
    },
  },
  plugins: [],
};
