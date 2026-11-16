// The SOAR mark: a shield of three stacked chevrons (detect, decide, respond).
export function Logo({ size = 22 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" aria-hidden>
      <rect width="24" height="24" rx="6" fill="rgb(var(--accent))" />
      <path d="M7 8.5 12 6l5 2.5" stroke="rgb(var(--accent-fg))" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" />
      <path d="M7 12.5 12 10l5 2.5" stroke="rgb(var(--accent-fg))" strokeOpacity=".75" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" />
      <path d="M7 16.5 12 14l5 2.5" stroke="rgb(var(--accent-fg))" strokeOpacity=".5" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}
