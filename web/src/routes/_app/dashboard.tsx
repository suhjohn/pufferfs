import { createFileRoute } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { createCLIKey, fetchAPIKeys, fetchRoots } from "../../lib/queries";

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

  return (
    <main className="container">
      <div className="page-heading">
        <h1>dashboard</h1>
      </div>
      <section className="card setup-card" aria-label="CLI setup">
        <div className="row setup-row">
          <div>
            <h2>CLI setup</h2>
            <p className="muted">
              Run init to sign in in your browser and auto-create a scoped CLI
              key. Use this button only when you need a manual key.
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
            <pre className="setup-code">{`# macOS (Homebrew)
brew install --cask suhjohn/tap/pufferfs

# macOS / Linux (installer script)
curl -fsSL https://pufferfs.com/install.sh | sh

# then configure
pufferfs init --api-key ${newKey}`}</pre>
          </>
        )}
        {!newKey && (
          <pre className="setup-code">{`# macOS (Homebrew)
brew install --cask suhjohn/tap/pufferfs

# macOS / Linux (installer script)
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
      {isPending && <p className="muted">loading</p>}
      {isError && <p className="muted">error: {(error as Error).message}</p>}
      {roots && roots.length === 0 && <p className="muted">no roots yet</p>}
      {roots && roots.length > 0 && (
        <div className="list">
          {roots.map((root) => (
            <div key={root.id} className="list-item">
              <strong>{root.name}</strong>
              <div className="muted">{root.id}</div>
            </div>
          ))}
        </div>
      )}
    </main>
  );
}
