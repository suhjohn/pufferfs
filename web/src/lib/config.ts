// Build-time feature flags. Set in .env / Pulumi web build env.

/** Base URL of the Go API (the api.* subdomain in prod). */
export const API_URL = import.meta.env.VITE_API_URL ?? "http://localhost:8080";

/**
 * Whether the payment/billing surface is enabled for this deployment.
 * When false, the Billing nav item + /billing route are hidden and no Stripe
 * configuration is required anywhere in the stack.
 */
export const BILLING_ENABLED = import.meta.env.VITE_ENABLE_BILLING === "true";
