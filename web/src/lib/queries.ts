import { api } from "./api";

export interface Root {
  id: string;
  org_id: string;
  name: string;
  source_path: string;
  scope: "org" | "user" | string;
  owner_user_id?: string;
  visible_generation_id: string;
  visible_generation_seq: number;
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

export interface SyncJob {
  id: string;
  org_id: string;
  root_id: string;
  user_id: string;
  status: string;
  total_files: number;
  processed: number;
  errors?: unknown;
  started_at: string;
  finished_at?: string;
}

export interface RootSyncSummary {
  root: Root;
  latestJob?: SyncJob;
}

// The Go API returns JSON `null` for empty slices, so every list query coalesces
// to [] to keep components total.
export async function fetchRoots(): Promise<Root[]> {
  return (await api<Root[] | null>("/roots")) ?? [];
}

export async function deleteRoot(rootId: string): Promise<void> {
  await api(`/roots/${rootId}`, { method: "DELETE" });
}

export async function fetchSyncJobs(rootId: string): Promise<SyncJob[]> {
  return (await api<SyncJob[] | null>(`/roots/${rootId}/sync/jobs`)) ?? [];
}

export async function fetchRootSyncSummaries(
  roots: Root[],
): Promise<RootSyncSummary[]> {
  const jobsByRoot = await Promise.all(
    roots.map(async (root) => ({
      root,
      jobs: await fetchSyncJobs(root.id).catch(() => []),
    })),
  );

  return jobsByRoot.map(({ root, jobs }) => ({
    root,
    latestJob: jobs[0],
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
