import { defineConfig } from "vitest/config";

// Two entries, no framework. `index.html` is the tower dashboard (served at
// `/`), `scan.html` is the phone relay (served at `/s/{sid}`). Output goes to
// web/dist, which embed.go at the repo root compiles into airlift-tower.
export default defineConfig({
  build: {
    rollupOptions: {
      input: {
        tower: "index.html",
        scan: "scan.html",
      },
    },
    target: "es2020",
    sourcemap: false,
  },
  server: {
    // Phase 3: proxy /api to a running tower and add @vitejs/plugin-basic-ssl.
  },
  test: {
    include: ["src/**/*.test.ts"],
  },
});
