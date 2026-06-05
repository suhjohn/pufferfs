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
