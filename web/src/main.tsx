import React, { useEffect } from "react";
import ReactDOM from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router-dom";
import { Toaster } from "sonner";
import "@fontsource-variable/inter";
import "@fontsource-variable/jetbrains-mono";
import { loadConfig } from "./lib/config";
import { restore } from "./lib/auth";
import { Problem } from "./lib/api";
import { applyAppearance, usePrefs } from "./lib/prefs";
import { TooltipProvider } from "./components/ui/tooltip";
import { ConfirmHost } from "./components/ui/dialog";
import App from "./App";
import "./index.css";

const qc = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 10_000,
      retry: (n, err) => !(err instanceof Problem && err.status < 500) && n < 2,
      refetchOnWindowFocus: false,
    },
  },
});

// Paint the stored theme before React renders, so there is no flash of the wrong theme.
applyAppearance(usePrefs.getState().theme, usePrefs.getState().density);

function Appearance() {
  const theme = usePrefs((s) => s.theme);
  const density = usePrefs((s) => s.density);
  useEffect(() => {
    applyAppearance(theme, density);
    if (theme !== "system") return;
    const m = window.matchMedia("(prefers-color-scheme: dark)");
    const on = () => applyAppearance(theme, density);
    m.addEventListener("change", on);
    return () => m.removeEventListener("change", on);
  }, [theme, density]);
  const dark = theme === "dark" || (theme === "system" && window.matchMedia?.("(prefers-color-scheme: dark)").matches);
  return (
    <Toaster
      theme={dark ? "dark" : "light"}
      position="bottom-right"
      closeButton
      toastOptions={{ classNames: { toast: "!border-line-strong !bg-raised !text-fg !shadow-pop !font-sans", description: "!text-fg-3", actionButton: "!bg-accent !text-accent-fg" } }}
    />
  );
}

loadConfig()
  .then(() => restore().catch(() => false))
  .finally(() => {
    ReactDOM.createRoot(document.getElementById("root")!).render(
      <React.StrictMode>
        <QueryClientProvider client={qc}>
          <TooltipProvider>
            <BrowserRouter>
              <App />
              <Appearance />
              <ConfirmHost />
            </BrowserRouter>
          </TooltipProvider>
        </QueryClientProvider>
      </React.StrictMode>,
    );
  });
