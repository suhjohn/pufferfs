import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tsConfigPaths from "vite-tsconfig-paths";
import { tanstackStart } from "@tanstack/react-start/plugin/vite";

// TanStack Start in SPA mode → emits a single route-agnostic `_shell.html`
// that CloudFront serves for every path (see infra/pulumi/index.ts). This is
// what makes full-page deep-loads (e.g. the OAuth callback landing on
// /auth/callback) render cleanly with no hydration mismatch.
//
// SEO: the shell carries the landing's <title>/description/og meta (defined on
// the root route in __root.tsx), and the body renders client-side. No Node SSR
// server to operate — it's still a static folder on S3 + CloudFront.
export default defineConfig({
  plugins: [
    tsConfigPaths(),
    tanstackStart({
      spa: { enabled: true },
    }),
    react(),
  ],
});
