import { createFileRoute } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import {
  createCLIKey,
  fetchAPIKeys,
  fetchRoots,
  fetchRootSyncSummaries,
} from "../../lib/queries";

export const Route = createFileRoute("/_app/dashboard")({
  component: Dashboard,
});

function Dashboard() {
  const queryClient = useQueryClient();
  const [newKey, setNewKey] = useState("");
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
    mutationFn: createCLIKey,
    onSuccess: (key) => {
      setNewKey(key);
      queryClient.invalidateQueries({ queryKey: ["api-keys"] });
    },
  });

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

      <section className="console-section" aria-label="CLI setup">
        <div className="section-heading">
          <div>
            <h2>CLI setup</h2>
            <p>
              Run init to sign in in your browser and auto-create a scoped CLI
              key. Create a manual key only for CI or secret-manager injection.
            </p>
          </div>
          <button
            className="btn btn-sm"
            disabled={createKey.isPending}
            onClick={() => createKey.mutate()}
          >
            {createKey.isPending ? "creating" : "create CLI key"}
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
        {!newKey && (
          <pre className="setup-code">{`# macOS / Linux
curl -fsSL https://pufferfs.com/install.sh | sh

# then configure
pufferfs init`}</pre>
        )}
        {keysQuery.data && keysQuery.data.length > 0 && (
          <p className="muted">
            {keysQuery.data.length} API key
            {keysQuery.data.length === 1 ? "" : "s"} already created for this
            account.
          </p>
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
            <div className="data-row data-row-head">
              <span>name</span>
              <span>scope</span>
              <span>last synced</span>
              <span>status</span>
            </div>
            {rootSummaries.map(({ root, latestJob }) => (
              <div key={root.id} className="data-row root-data-row">
                <strong>{root.name}</strong>
                <span className="tag">{root.scope}</span>
                <span className="muted">
                  {formatDateTime(latestJob?.finished_at ?? latestJob?.started_at)}
                </span>
                <span className="status-text">{rootStatus(root.visible_generation_id, latestJob?.status)}</span>
              </div>
            ))}
          </div>
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
