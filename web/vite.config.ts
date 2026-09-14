import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import basicSsl from "@vitejs/plugin-basic-ssl";
import type { Plugin, ViteDevServer } from "vite";
import { defineConfig } from "vitest/config";

// A running tower to proxy /api to during `vite dev`.
const tower = process.env.AIRLIFT_TOWER ?? "https://127.0.0.1:8443";
// basic-ssl gives the phone a secure context on the LAN; AIRLIFT_HTTP=1
// turns it off for plain-HTTP localhost work (localhost is secure anyway).
const ssl = process.env.AIRLIFT_HTTP !== "1";

const BASE_SENTINEL = "<!--airlift-base-->";

/**
 * Fills the base sentinel with <base href="/"> for dev and preview, the way the
 * tower does at serve time in production (with the real prefix). apply:"serve"
 * keeps the build output's raw sentinel intact for the Go server to rewrite.
 */
function injectBase(): Plugin {
  const withBase = (html: string) => html.replace(BASE_SENTINEL, '<base href="/">');
  return {
    name: "airlift-base",
    apply: "serve",
    transformIndexHtml: { order: "pre", handler: withBase }, // dev
    configurePreviewServer(server) {
      server.middlewares.use((req, _res, next) => {
        const p = (req.url ?? "").split("?")[0];
        const file =
          p === "/" || p === "/index.html"
            ? "index.html"
            : p === "/scan.html"
              ? "scan.html"
              : p === "/admin.html"
                ? "admin.html"
                : p === "/docs.html"
                  ? "docs.html"
                  : null;
        if (!file) return next();
        try {
          const html = readFileSync(resolve("dist", file), "utf8");
          _res.setHeader("Content-Type", "text/html; charset=utf-8");
          _res.end(withBase(html));
        } catch {
          next();
        }
      });
    },
  };
}

/** Serves scan.html at /s/{sid} and admin.html at /admin in dev and preview, as
 *  the tower does. */
function scanRoute(): Plugin {
  const rewrite = (server: ViteDevServer | { middlewares: ViteDevServer["middlewares"] }) => {
    server.middlewares.use((req, _res, next) => {
      if (req.url && /^\/s\/[^/?#]+\/?(\?.*)?$/.test(req.url)) req.url = "/scan.html";
      else if (req.url && /^\/admin\/?(\?.*)?$/.test(req.url)) req.url = "/admin.html";
      else if (req.url && /^\/docs\/?(\?.*)?$/.test(req.url)) req.url = "/docs.html";
      next();
    });
  };
  return { name: "airlift-scan-route", configureServer: rewrite, configurePreviewServer: rewrite };
}

const proxy = {
  "/api": { target: tower, secure: false, changeOrigin: true },
};

export default defineConfig({
  // Relative asset URLs, so the built pages work under any path prefix once the
  // tower injects <base href> (ADR 0012).
  base: "./",
  plugins: [scanRoute(), injectBase(), ...(ssl ? [basicSsl()] : [])],
  build: {
    rollupOptions: {
      input: { tower: "index.html", scan: "scan.html", admin: "admin.html", docs: "docs.html" },
    },
    target: "es2020",
    sourcemap: false,
  },
  server: { proxy },
  preview: { proxy },
  test: { include: ["src/**/*.test.ts"] },
});
