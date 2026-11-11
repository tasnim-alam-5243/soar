import React from "react";
import ReactDOM from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router-dom";
import { loadConfig } from "./lib/config";
import { restore } from "./lib/auth";
import { Problem } from "./lib/api";
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

if (localStorage.getItem("soar.theme") === "dark") document.documentElement.classList.add("dark");

loadConfig()
  .then(() => restore().catch(() => false))
  .finally(() => {
    ReactDOM.createRoot(document.getElementById("root")!).render(
      <React.StrictMode>
        <QueryClientProvider client={qc}>
          <BrowserRouter>
            <App />
          </BrowserRouter>
        </QueryClientProvider>
      </React.StrictMode>,
    );
  });
