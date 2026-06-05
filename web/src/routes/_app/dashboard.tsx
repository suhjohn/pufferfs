import { createFileRoute } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { fetchRoots } from "../../lib/queries";

export const Route = createFileRoute("/_app/dashboard")({
  component: Dashboard,
});

function Dashboard() {
  const { data: roots, isPending, isError, error } = useQuery({
    queryKey: ["roots"],
    queryFn: fetchRoots,
  });

  return (
    <main className="container">
      <h1 className="prompt">dashboard</h1>
      {isPending && <p className="muted">// loading…</p>}
      {isError && <p className="muted">// error: {(error as Error).message}</p>}
      {roots && roots.length === 0 && <p className="muted">// no roots yet</p>}
      {roots?.map((root) => (
        <div key={root.id} className="card">
          <strong>{root.name}</strong>
          <div className="muted">{root.id}</div>
        </div>
      ))}
    </main>
  );
}
