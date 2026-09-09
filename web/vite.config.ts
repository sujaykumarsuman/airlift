import basicSsl from "@vitejs/plugin-basic-ssl";
import type { Plugin, ViteDevServer } from "vite";
import { defineConfig } from "vitest/config";

// A running tower to proxy /api and /ca.crt to during `vite dev`.
const tower = process.env.AIRLIFT_TOWER ?? "https://127.0.0.1:8443";
// basic-ssl gives the phone a secure context on the LAN; AIRLIFT_HTTP=1
// turns it off for plain-HTTP localhost work (localhost is secure anyway).
const ssl = process.env.AIRLIFT_HTTP !== "1";

/** Serves scan.html at /s/{sid} in dev and preview, as the tower does. */
function scanRoute(): Plugin {
  const rewrite = (server: ViteDevServer | { middlewares: ViteDevServer["middlewares"] }) => {
    server.middlewares.use((req, _res, next) => {
      if (req.url && /^\/s\/[^/?#]+\/?(\?.*)?$/.test(req.url)) req.url = "/scan.html";
      next();
    });
  };
  return { name: "airlift-scan-route", configureServer: rewrite, configurePreviewServer: rewrite };
}

const proxy = {
  "/api": { target: tower, secure: false, changeOrigin: true },
  "/ca.crt": { target: tower, secure: false, changeOrigin: true },
};

export default defineConfig({
  plugins: [scanRoute(), ...(ssl ? [basicSsl()] : [])],
  build: {
    rollupOptions: {
      input: { tower: "index.html", scan: "scan.html" },
    },
    target: "es2020",
    sourcemap: false,
  },
  server: { proxy },
  preview: { proxy },
  test: { include: ["src/**/*.test.ts"] },
});
