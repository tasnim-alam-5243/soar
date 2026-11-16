// One severity scale everywhere (1..5). Color is never the only cue: every place that shows a
// severity also shows its name or the bar glyph.

export const SEVERITIES = [5, 4, 3, 2, 1] as const;
export type Severity = 1 | 2 | 3 | 4 | 5;

export const sevName: Record<number, string> = { 1: "Info", 2: "Low", 3: "Medium", 4: "High", 5: "Critical" };

export const sevColor = (s: number) => `rgb(var(--sev-${Math.min(5, Math.max(1, s || 1))}))`;

export const clampSev = (s?: number | null): Severity => Math.min(5, Math.max(1, Math.round(s ?? 1))) as Severity;

// Alert triage vocabulary (matches the API).
export const ALERT_STATUSES = ["new", "acknowledged", "in_progress", "closed"] as const;
export const OPEN_ALERT_STATUSES = "new,acknowledged,in_progress";
export const VERDICTS = ["true_positive", "false_positive", "benign", "unknown"] as const;
export const verdictName: Record<string, string> = {
  true_positive: "True positive",
  false_positive: "False positive",
  benign: "Benign",
  unknown: "Unknown",
};
