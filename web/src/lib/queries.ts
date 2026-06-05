import { api } from "./api";

export interface Root {
  id: string;
  name: string;
}

export interface Org {
  id: string;
  name: string;
}

export interface Member {
  userId: string;
  email: string;
  role: string;
}

export interface Billing {
  plan: string;
  status: string;
  currentPeriodEnd?: string;
}

// The Go API returns JSON `null` for empty slices, so every list query coalesces
// to [] to keep components total.
export async function fetchRoots(): Promise<Root[]> {
  return (await api<Root[] | null>("/roots")) ?? [];
}

export function fetchOrg(): Promise<Org> {
  return api<Org>("/org");
}

export async function fetchMembers(): Promise<Member[]> {
  return (await api<Member[] | null>("/org/members")) ?? [];
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
