import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import path from "node:path";

// The dev server proxies the API so the UI runs on one origin, as behind nginx.
export default defineConfig({
  plugins: [react()],
  resolve: { alias: { "@": path.resolve(__dirname, "src") } },
  server: {
    port: 3000,
    proxy: {
      "/api": { target: process.env.SOAR_API ?? "http://127.0.0.1:8080", changeOrigin: true },
    },
  },
  build: {
    chunkSizeWarningLimit: 4000,
    rollupOptions: { output: { manualChunks: { flow: ["@xyflow/react", "@dagrejs/dagre"] } } },
  },
  test: { environment: "jsdom" },
} as never);
