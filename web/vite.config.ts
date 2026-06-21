import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

const backendPort = process.env.BACKEND_PORT ?? "8080";
const backend = `http://localhost:${backendPort}`;
const backendWs = `ws://localhost:${backendPort}`;

export default defineConfig({
  plugins: [react()],
  server: {
    port: parseInt(process.env.FRONTEND_PORT ?? "3000"),
    strictPort: false,   // auto-increment if port is taken
    proxy: {
      "/api": backend,
      "/terminal": { target: backendWs, ws: true },
    },
  },
  build: {
    outDir: "dist",
    sourcemap: false,
    rollupOptions: {
      output: {
        // Split xterm into its own chunk so the main bundle stays small.
        manualChunks: { xterm: ["@xterm/xterm", "@xterm/addon-fit"] },
      },
    },
  },
});
