import { API_URL } from "./config";

export class ApiError extends Error {
  constructor(
    public status: number,
    public body: string,
  ) {
    super(`API ${status}: ${body}`);
    this.name = "ApiError";
  }
}

/**
 * Thin typed wrapper around the Go API. Sends the auth cookie
 * (httpOnly, set by the backend's OAuth callback) on every request via
 * `credentials: "include"`, so the API must allow this origin with
 * Access-Control-Allow-Credentials.
 */
export async function api<T = unknown>(
  path: string,
  init: RequestInit = {},
): Promise<T> {
  const res = await fetch(`${API_URL}${path}`, {
    credentials: "include",
    headers: {
      "Content-Type": "application/json",
      ...(init.headers ?? {}),
    },
    ...init,
  });

  if (!res.ok) {
    throw new ApiError(res.status, await res.text().catch(() => ""));
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}
