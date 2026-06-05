# PufferFS Web

Landing + login + organization + dashboard (+ optional billing) frontend for
PufferFS. Built with **Vite + TanStack Start** (file-based routing, SSR
framework run in static/prerender mode).

## Architecture

- **Landing (`/`)** is prerendered to static HTML at build time for SEO.
- **Authed pages** (`/dashboard`, `/organization`, `/billing`) live under the
  pathless `_app` layout and hydrate client-side as a SPA. The layout's
  `beforeLoad` calls `GET /auth/me` and redirects to `/login` on 401.
- **Auth** is owned by the Go API: `/login` links to `{API}/auth/google`; the
  backend's OAuth callback sets an httpOnly cookie (`Domain=.pufferfs.com`,
  `Secure`, `SameSite=Lax`) and redirects to `/auth/callback` here.
- The browser talks to the API on the `api.*` subdomain with
  `credentials: "include"` (see `src/lib/api.ts`).

## Payment is optional

The whole billing surface is gated behind `VITE_ENABLE_BILLING`:

- `false` (default): Billing nav + `/billing` route are hidden; no Stripe
  config needed anywhere. The matching Pulumi flag is `enableBilling=false`.
- `true`: requires the Go API to expose `/billing`,
  `/billing/checkout-session`, `/billing/webhook`, and the Stripe secrets set
  in Pulumi (`stripeSecretKey`, `stripeWebhookSecret`).

## Develop

```sh
cp .env.example .env   # set VITE_API_URL, VITE_ENABLE_BILLING
npm install
npm run dev            # http://localhost:3000
```

## Build & deploy

Requires **Node 20+** (Vite 7). The build prerenders `/` to a crawlable
`dist/client/index.html` and emits the SPA bundle; everything else is served
from that `index.html` via CloudFront's fallback.

```sh
npm run build          # static site → dist/client/
aws s3 sync dist/client/ s3://$WEB_BUCKET --delete
aws cloudfront create-invalidation --distribution-id $CDN_ID --paths '/*'
```

> `dist/server/` is only used by the prerender step at build time; don't upload it.

`$WEB_BUCKET` and the CloudFront distribution are created by
`infra/pulumi` (outputs `webBucketName` and `webUrl`).
