import { createFileRoute } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { capture } from "../../lib/analytics";
import {
  createAPIKey,
  deleteRoot,
  fetchAPIKeys,
  fetchRoots,
  fetchRootSyncSummaries,
  type Root,
  revokeAPIKey,
} from "../../lib/queries";

export const Route = createFileRoute("/_app/dashboard")({
  component: Dashboard,
});

function Dashboard() {
  const queryClient = useQueryClient();
  const trackedView = useRef(false);
  const [newKey, setNewKey] = useState("");
  const [keyName, setKeyName] = useState("CLI key");
  const [selectedScopes, setSelectedScopes] = useState<string[]>([
    "sync",
    "query",
  ]);
  const { data: roots, isPending, isError, error } = useQuery({
    queryKey: ["roots"],
    queryFn: fetchRoots,
  });
  const rootSyncsQuery = useQuery({
    queryKey: ["root-sync-summaries", roots?.map((root) => root.id).join(",")],
    queryFn: () => fetchRootSyncSummaries(roots ?? []),
    enabled: Boolean(roots?.length),
  });
  const keysQuery = useQuery({
    queryKey: ["api-keys"],
    queryFn: fetchAPIKeys,
  });
  const createKey = useMutation({
    mutationFn: () =>
      createAPIKey({
        name: keyName.trim() || "API key",
        scopes: selectedScopes,
      }),
    onSuccess: (key) => {
      setNewKey(key);
      capture("api_key_created", {
        scope_count: selectedScopes.length,
        has_query_scope: selectedScopes.includes("query"),
        has_sync_scope: selectedScopes.includes("sync"),
        has_delete_scope: selectedScopes.includes("root:delete"),
      });
      queryClient.invalidateQueries({ queryKey: ["api-keys"] });
    },
  });
  const revokeKey = useMutation({
    mutationFn: revokeAPIKey,
    onSuccess: () => {
      capture("api_key_revoked");
      queryClient.invalidateQueries({ queryKey: ["api-keys"] });
    },
  });
  const deleteRootMutation = useMutation({
    mutationFn: deleteRoot,
    onSuccess: (_data, rootId) => {
      const deletedRoot = roots?.find((root) => root.id === rootId);
      capture("root_deleted", {
        root_scope: deletedRoot?.scope,
        had_visible_generation: Boolean(deletedRoot?.visible_generation_id),
      });
      queryClient.invalidateQueries({ queryKey: ["roots"] });
      queryClient.invalidateQueries({ queryKey: ["root-sync-summaries"] });
    },
  });

  function confirmDeleteRoot(root: Root) {
    const ok = window.confirm(
      `Delete root "${root.name}"?\n\nThis removes indexed content and stored sync artifacts for the root. Sync job history is retained for accounting.`,
    );
    if (ok) deleteRootMutation.mutate(root.id);
  }

  const rootCount = roots?.length ?? 0;
  const keyCount = keysQuery.data?.length ?? 0;
  const rootSummaries =
    rootSyncsQuery.data ??
    roots?.map((root) => ({
      root,
      latestJob: undefined,
    })) ??
    [];
  const recentSyncs = rootSummaries
    .filter((summary) => summary.latestJob)
    .sort((a, b) => {
      const aStarted = a.latestJob?.started_at ?? "";
      const bStarted = b.latestJob?.started_at ?? "";
      return bStarted.localeCompare(aStarted);
    })
    .slice(0, 5);

  useEffect(() => {
    if (trackedView.current || !roots || !keysQuery.data) return;
    trackedView.current = true;
    capture("dashboard_viewed", {
      root_count: roots.length,
      api_key_count: keysQuery.data.length,
      has_roots: roots.length > 0,
      has_api_keys: keysQuery.data.length > 0,
    });
  }, [roots, keysQuery.data]);

  return (
    <main className="console-page">
      <header className="console-header">
        <div>
          <p className="eyebrow">console</p>
          <h1>dashboard</h1>
          <p className="muted">
            Connect the CLI, inspect synced roots, and manage agent access.
          </p>
        </div>
        <div className="console-metrics" aria-label="account summary">
          <div>
            <span>{rootCount}</span>
            <p>roots</p>
          </div>
          <div>
            <span>{keyCount}</span>
            <p>API keys</p>
          </div>
        </div>
      </header>

      <section className="console-section" aria-label="API keys">
        <div className="section-heading">
          <div>
            <h2>API keys</h2>
            <p>
              Create scoped keys for the CLI, CI, and local agents. The raw key
              is shown once after creation.
            </p>
          </div>
        </div>

        <div className="settings-grid">
          <label className="field-label">
            <span>name</span>
            <input
              value={keyName}
              onChange={(event) => setKeyName(event.target.value)}
              placeholder="CI deploy key"
            />
          </label>
          <div className="field-label">
            <span>permissions</span>
            <div className="checkbox-grid" role="group" aria-label="API key permissions">
              {API_KEY_SCOPES.map((scope) => (
                <label key={scope.value} className="check-row">
                  <input
                    type="checkbox"
                    checked={selectedScopes.includes(scope.value)}
                    onChange={() =>
                      setSelectedScopes((current) =>
                        current.includes(scope.value)
                          ? current.filter((value) => value !== scope.value)
                          : [...current, scope.value],
                      )
                    }
                  />
                  <span>{scope.label}</span>
                </label>
              ))}
            </div>
          </div>
          <button
            className="btn btn-sm"
            disabled={createKey.isPending || selectedScopes.length === 0}
            onClick={() => createKey.mutate()}
          >
            {createKey.isPending ? "creating" : "create key"}
          </button>
        </div>

        {createKey.isError && (
          <p className="muted">error: {(createKey.error as Error).message}</p>
        )}
        {newKey && (
          <>
            <p className="muted">
              Copy this now. PufferFS stores only the key hash and cannot show it
              again.
            </p>
            <pre className="setup-code">{`# macOS / Linux
curl -fsSL https://pufferfs.com/install.sh | sh

# then configure
pufferfs init --api-key ${newKey}`}</pre>
          </>
        )}

        {keysQuery.isPending && <p className="muted">loading keys</p>}
        {keysQuery.data && keysQuery.data.length === 0 && (
          <div className="empty-state">no API keys yet</div>
        )}
        {keysQuery.data && keysQuery.data.length > 0 && (
          <div className="data-list">
            <div className="data-row data-row-head key-data-row">
              <span>name</span>
              <span>permissions</span>
              <span>created</span>
              <span />
            </div>
            {keysQuery.data.map((key) => (
              <div key={key.id} className="data-row key-data-row">
                <strong>{key.name || "API key"}</strong>
                <span className="scope-list">
                  {key.scopes.map((scope) => (
                    <span key={scope} className="tag">
                      {scope}
                    </span>
                  ))}
                </span>
                <span className="muted">{formatDateTime(key.created_at)}</span>
                <button
                  className="btn btn-sm btn-danger"
                  disabled={revokeKey.isPending}
                  onClick={() => revokeKey.mutate(key.id)}
                >
                  revoke
                </button>
              </div>
            ))}
          </div>
        )}
      </section>

      <section className="console-section" aria-label="synced roots">
        <div className="section-heading">
          <div>
            <h2>roots</h2>
            <p>Folders that have been synced into a committed search index.</p>
          </div>
        </div>
        {isPending && <p className="muted">loading</p>}
        {isError && <p className="muted">error: {(error as Error).message}</p>}
        {roots && roots.length === 0 && (
          <div className="empty-state">no roots yet</div>
        )}
        {roots && roots.length > 0 && (
          <div className="data-list">
            <div className="data-row data-row-head root-data-row">
              <span>name</span>
              <span>scope</span>
              <span>last synced</span>
              <span>status</span>
              <span />
            </div>
            {rootSummaries.map(({ root, latestJob }) => (
              <div key={root.id} className="data-row root-data-row">
                <strong>{root.name}</strong>
                <span className="tag">{root.scope}</span>
                <span className="muted">
                  {formatDateTime(latestJob?.finished_at ?? latestJob?.started_at)}
                </span>
                <span className="status-text">{rootStatus(root.visible_generation_id, latestJob?.status)}</span>
                <button
                  className="btn btn-sm btn-danger"
                  disabled={deleteRootMutation.isPending}
                  onClick={() => confirmDeleteRoot(root)}
                >
                  delete
                </button>
              </div>
            ))}
          </div>
        )}
        {deleteRootMutation.isError && (
          <p className="muted">error: {(deleteRootMutation.error as Error).message}</p>
        )}
      </section>

      <section className="console-section" aria-label="recent syncs">
        <div className="section-heading">
          <div>
            <h2>recent syncs</h2>
            <p>Latest sync activity across your roots.</p>
          </div>
        </div>
        {rootSyncsQuery.isPending && roots && roots.length > 0 && (
          <p className="muted">loading</p>
        )}
        {recentSyncs.length === 0 && !rootSyncsQuery.isPending && (
          <div className="empty-state">no syncs yet</div>
        )}
        {recentSyncs.length > 0 && (
          <div className="data-list">
            <div className="data-row data-row-head sync-data-row">
              <span>root</span>
              <span>status</span>
              <span>progress</span>
              <span>started</span>
            </div>
            {recentSyncs.map(({ root, latestJob }) => (
              <div key={latestJob?.id ?? root.id} className="data-row sync-data-row">
                <strong>{root.name}</strong>
                <span className="status-text">{latestJob?.status ?? "unknown"}</span>
                <span className="muted">
                  {formatProgress(latestJob?.processed, latestJob?.total_files)}
                </span>
                <span className="muted">{formatDateTime(latestJob?.started_at)}</span>
              </div>
            ))}
          </div>
        )}
      </section>
    </main>
  );
}

const API_KEY_SCOPES = [
  { value: "query", label: "query" },
  { value: "sync", label: "sync" },
  { value: "root:delete", label: "delete roots" },
  { value: "api_keys:read", label: "read keys" },
  { value: "api_keys:write", label: "write keys" },
  { value: "org:admin", label: "manage org" },
];

function rootStatus(hasVisibleGeneration: string, latestStatus?: string) {
  if (latestStatus && latestStatus !== "completed") return latestStatus;
  if (hasVisibleGeneration) return "synced";
  return latestStatus ?? "not synced";
}

function formatProgress(processed?: number, total?: number) {
  if (!total) return "0/0";
  return `${processed ?? 0}/${total}`;
}

function formatDateTime(value?: string) {
  if (!value) return "never";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "unknown";
  return date.toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
  });
}
