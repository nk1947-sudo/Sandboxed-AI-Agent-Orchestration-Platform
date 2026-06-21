import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 3000,
    proxy: {
      "/api": "http://localhost:8080",
      "/terminal": { target: "ws://localhost:8080", ws: true },
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
