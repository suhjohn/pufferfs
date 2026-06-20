import { api } from "./api";

export interface Session {
  userId: string;
  email: string;
  name?: string;
  orgId: string;
  role: string;
}

// Shape returned by the Go API's GET /auth/me.
interface MeResponse {
  user: { id: string; email: string; name?: string };
  org_id: string;
  role: string;
}

/** Resolves the current session from the auth cookie, or throws (401). */
export async function fetchSession(): Promise<Session> {
  const me = await api<MeResponse>("/auth/me");
  return {
    userId: me.user.id,
    email: me.user.email,
    name: me.user.name,
    orgId: me.org_id,
    role: me.role,
  };
}

/** Clears the session cookie server-side. */
export function logout(): Promise<void> {
  return api<void>("/auth/logout", { method: "POST" }).catch(() => undefined);
}

export interface EmailLoginStartResponse {
  challenge_id: string;
  expires_in: number;
  resend_after: number;
}

export function startEmailLogin(input: {
  email: string;
  flow?: "web" | "cli";
  cli_redirect_uri?: string;
}): Promise<EmailLoginStartResponse> {
  return api<EmailLoginStartResponse>("/auth/email/start", {
    method: "POST",
    body: JSON.stringify(input),
  });
}

export function verifyEmailLogin(input: {
  challenge_id: string;
  code: string;
}): Promise<{ status: string }> {
  return api<{ status: string }>("/auth/email/verify", {
    method: "POST",
    body: JSON.stringify(input),
  });
}

export interface AuthProviders {
  email_code: boolean;
  google: boolean;
}

export function fetchAuthProviders(): Promise<AuthProviders> {
  return api<AuthProviders>("/auth/providers");
}
