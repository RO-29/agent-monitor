import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

const T = process.env.AM_TARGET || "127.0.0.1:7778";

// Built output is embedded into the Go binary (//go:embed web/dist).
// base "./" keeps asset URLs relative so the daemon can serve them from any path.
export default defineConfig({
  plugins: [react()],
  base: "/",
  build: { outDir: "dist", emptyOutDir: true, sourcemap: false, target: "es2020" },
  server: {
    port: 5173,
    proxy: {
      // The daemon refuses cross-origin writes, so the proxy presents the daemon's own origin.
      "/api": { target: `http://${T}`, ws: true, changeOrigin: true, headers: { origin: `http://${T}` } },
      "/events": { target: `http://${T}`, changeOrigin: true, headers: { origin: `http://${T}` } },
      "/ws": { target: `ws://${T}`, ws: true, changeOrigin: true, headers: { origin: `http://${T}` } },
    },
  },
});
