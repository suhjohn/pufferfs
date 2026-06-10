import { POSTHOG_HOST, POSTHOG_KEY } from "./config";
import type { Session } from "./auth";

type AnalyticsValue = string | number | boolean | null | undefined;
type AnalyticsProperties = Record<string, AnalyticsValue>;

let posthogPromise:
  | Promise<typeof import("posthog-js").default | null>
  | null = null;

function isEnabled() {
  return Boolean(POSTHOG_KEY) && typeof window !== "undefined";
}

async function client() {
  if (!isEnabled()) return null;
  if (!posthogPromise) {
    posthogPromise = import("posthog-js").then(({ default: posthog }) => {
      posthog.init(POSTHOG_KEY, {
        api_host: POSTHOG_HOST,
        capture_pageview: false,
        autocapture: false,
        disable_session_recording: true,
      });
      return posthog;
    });
  }
  return posthogPromise;
}

export function capture(event: string, properties: AnalyticsProperties = {}) {
  void client().then((posthog) => {
    posthog?.capture(event, compactProperties(properties));
  });
}

export function capturePageview(pathname: string) {
  void client().then((posthog) => {
    if (!posthog || typeof window === "undefined") return;
    posthog.capture("$pageview", {
      $current_url: `${window.location.origin}${pathname}`,
      $pathname: pathname,
      page_title: document.title,
    });
  });
}

export function identifySession(session: Session) {
  void client().then((posthog) => {
    if (!posthog) return;
    posthog.identify(session.userId, {
      role: session.role,
      email_domain: emailDomain(session.email),
      has_name: Boolean(session.name),
    });
    posthog.group("organization", session.orgId);
  });
}

export function resetAnalytics() {
  void client().then((posthog) => {
    posthog?.reset();
  });
}

function compactProperties(properties: AnalyticsProperties) {
  return Object.fromEntries(
    Object.entries(properties).filter(([, value]) => value !== undefined),
  );
}

function emailDomain(email: string) {
  const [, domain] = email.split("@");
  return domain || "unknown";
}
