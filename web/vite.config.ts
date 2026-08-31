import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Built output is embedded into the Go binary (//go:embed web/dist).
// base "./" keeps asset URLs relative so the daemon can serve them from any path.
export default defineConfig({
  plugins: [react()],
  base: "/",
  build: { outDir: "dist", emptyOutDir: true, sourcemap: false, target: "es2020" },
  server: {
    port: 5173,
    proxy: {
      "/api": "http://127.0.0.1:7778",
      "/events": "http://127.0.0.1:7778",
      "/ws": { target: "ws://127.0.0.1:7778", ws: true },
    },
  },
});
