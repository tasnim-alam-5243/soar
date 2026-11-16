import { useEffect, useRef } from "react";

// Keyboard shortcuts. Keys are single characters ("j", "/", "?"), chords with mod ("mod+k"),
// or two-key sequences ("g a"). Shortcuts never fire while the user types in a field.

type Handler = (e: KeyboardEvent) => void;

const typing = (e: KeyboardEvent) => {
  const t = e.target as HTMLElement | null;
  if (!t) return false;
  const tag = t.tagName;
  return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || t.isContentEditable || !!t.closest(".monaco-editor");
};

export const isMac = typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform);
export const modKey = isMac ? "⌘" : "Ctrl";

export function useHotkeys(map: Record<string, Handler>, deps: unknown[] = [], enabled = true) {
  const ref = useRef(map);
  ref.current = map;
  useEffect(() => {
    if (!enabled) return;
    let prefix: string | null = null;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const onKey = (e: KeyboardEvent) => {
      const keys = ref.current;
      const mod = e.metaKey || e.ctrlKey;
      const k = e.key.length === 1 ? e.key.toLowerCase() : e.key;
      if (mod) {
        const h = keys[`mod+${k}`];
        if (h) {
          e.preventDefault();
          h(e);
        }
        return;
      }
      if (e.altKey || e.defaultPrevented || typing(e)) return;
      // an open dialog, popover or menu owns the keyboard
      if (document.querySelector('[role="dialog"][data-state="open"], [role="menu"][data-state="open"], [role="alertdialog"]')) return;
      if (prefix) {
        const h = keys[`${prefix} ${k}`];
        prefix = null;
        clearTimeout(timer);
        if (h) {
          e.preventDefault();
          h(e);
          return;
        }
      }
      if (Object.keys(keys).some((x) => x.startsWith(k + " "))) {
        prefix = k;
        timer = setTimeout(() => (prefix = null), 900);
        return;
      }
      const h = keys[e.key === "?" ? "?" : k];
      if (h) {
        e.preventDefault();
        h(e);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => {
      window.removeEventListener("keydown", onKey);
      clearTimeout(timer);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [enabled, ...deps]);
}

/** The shortcut reference shown by "?". */
export const SHORTCUTS: { group: string; items: [string, string][] }[] = [
  {
    group: "Anywhere",
    items: [
      [`${modKey} K`, "Command palette and search"],
      ["?", "Keyboard shortcuts"],
      ["g d", "Go to dashboard"],
      ["g a", "Go to alerts"],
      ["g c", "Go to cases"],
      ["g w", "Go to workflows"],
      ["g e", "Go to executions"],
      ["g p", "Go to approvals"],
    ],
  },
  {
    group: "Lists",
    items: [
      ["j / k", "Next / previous row"],
      ["Enter", "Open the row"],
      ["x", "Select the row"],
      ["/", "Search"],
      ["n", "New case or workflow"],
      ["Esc", "Close the panel or clear the selection"],
    ],
  },
  {
    group: "Alert triage",
    items: [
      ["a", "Acknowledge"],
      ["i", "Assign to me"],
      ["c", "Close with a verdict"],
      ["o", "Open or link a case"],
    ],
  },
];
