import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import path from "node:path";

const api = process.env.SOAR_API ?? "http://127.0.0.1:8080";

// The production bundle goes to internal/webui/dist, where `go build -tags ui` embeds it into
// the soar binary (make build). The dev server proxies the API and its runtime config, so the
// UI runs on one origin exactly as it does when the api role serves it.
export default defineConfig({
  plugins: [react()],
  resolve: { alias: { "@": path.resolve(__dirname, "src") } },
  server: {
    port: 3000,
    proxy: {
      "/api": { target: api, changeOrigin: true },
      "/config.json": { target: api, changeOrigin: true },
    },
  },
  build: {
    outDir: path.resolve(__dirname, "../internal/webui/dist"),
    emptyOutDir: true,
    chunkSizeWarningLimit: 4000,
    rollupOptions: {
      output: {
        manualChunks: {
          flow: ["@xyflow/react", "@dagrejs/dagre"],
          charts: ["recharts"],
        },
      },
    },
  },
  test: { environment: "jsdom" },
} as never);
