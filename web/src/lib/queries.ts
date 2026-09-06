import { api } from "./api";

export interface Root {
  id: string;
  org_id: string;
  name: string;
  source_path: string;
  scope: "org" | "user" | string;
  owner_user_id?: string;
  access?: string[];
  access_source?: string;
  created_at: string;
  updated_at: string;
}

export interface Org {
  id: string;
  name: string;
}

export interface Member {
  user_id: string;
  email: string;
  name: string;
  avatar_url: string;
  role: string;
  joined_at: string;
}

export interface OrgInvite {
  id: string;
  email: string;
  role: string;
  invited_by_user_id: string;
  created_at: string;
}

export interface Billing {
  plan: string;
  status: string;
  currentPeriodEnd?: string;
}

export interface APIKey {
  id: string;
  name: string;
  scopes: string[];
  created_at: string;
  expires_at?: string;
}

export interface RootSyncSummary {
  root: Root;
  status: string;
  indexed: number;
  total: number;
}

// The Go API returns JSON `null` for empty slices, so every list query coalesces
// to [] to keep components total.
export async function fetchRoots(): Promise<Root[]> {
  return (await api<Root[] | null>("/roots")) ?? [];
}

export async function deleteRoot(rootId: string): Promise<void> {
  await api(`/roots/${rootId}`, { method: "DELETE" });
}

export async function fetchRootSyncSummaries(roots: Root[]): Promise<RootSyncSummary[]> {
  return Promise.all(roots.map(async (root) => {
    const summary: RootSyncSummary = { root, status: "not synced", indexed: 0, total: 0 };
    if (!root.access?.includes("sync")) {
      return { ...summary, status: "sync access required" };
    }
    try {
      let cursor = "";
      let failed = false;
      do {
        const page = await api<{
          files: { processing?: { status: string } }[];
          next_cursor?: string;
        }>(`/roots/${encodeURIComponent(root.id)}/captured-files?processing=true&limit=1000&cursor=${encodeURIComponent(cursor)}`);
        for (const file of page.files) {
          summary.total++;
          if (file.processing?.status === "complete") summary.indexed++;
          if (file.processing?.status === "failed") failed = true;
        }
        cursor = page.next_cursor ?? "";
      } while (cursor);
      summary.status = failed ? "failed" : summary.total === 0 ? "not synced"
        : summary.indexed === summary.total ? "indexed" : "processing";
    } catch {
      summary.status = "status unavailable";
    }
    return summary;
  }));
}

export function fetchOrg(): Promise<Org> {
  return api<Org>("/org");
}

export async function fetchMembers(): Promise<Member[]> {
  return (await api<Member[] | null>("/org/members")) ?? [];
}

export async function fetchOrgInvites(): Promise<OrgInvite[]> {
  return (await api<OrgInvite[] | null>("/org/invites")) ?? [];
}

export async function inviteOrgMember(input: {
  email: string;
  role: string;
}): Promise<OrgInvite> {
  return api<OrgInvite>("/org/invites", {
    method: "POST",
    body: JSON.stringify(input),
  });
}

export async function deleteOrgInvite(id: string): Promise<void> {
  await api(`/org/invites/${id}`, { method: "DELETE" });
}

export async function updateMemberRole(input: {
  userId: string;
  role: string;
}): Promise<Member> {
  return api<Member>(`/org/members/${input.userId}`, {
    method: "PUT",
    body: JSON.stringify({ role: input.role }),
  });
}

export async function removeOrgMember(userId: string): Promise<void> {
  await api(`/org/members/${userId}`, { method: "DELETE" });
}

export async function fetchAPIKeys(): Promise<APIKey[]> {
  return (await api<APIKey[] | null>("/auth/api-keys")) ?? [];
}

export async function createAPIKey(input: {
  name: string;
  scopes: string[];
}): Promise<string> {
  const { key } = await api<{ key: string }>("/auth/api-keys", {
    method: "POST",
    body: JSON.stringify(input),
  });
  return key;
}

export function createCLIKey(): Promise<string> {
  return createAPIKey({
    name: "CLI key",
    scopes: ["sync", "query", "root:delete"],
  });
}

export async function revokeAPIKey(id: string): Promise<void> {
  await api(`/auth/api-keys/${id}`, { method: "DELETE" });
}

export async function fetchBilling(): Promise<Billing> {
  return (
    (await api<Billing | null>("/billing")) ?? { plan: "free", status: "none" }
  );
}

export async function createCheckoutSession(): Promise<string> {
  const { url } = await api<{ url: string }>("/billing/checkout-session", {
    method: "POST",
    body: JSON.stringify({ plan: "pro" }),
  });
  return url;
}
