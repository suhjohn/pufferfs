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
      <div className="page-heading">
        <h1>dashboard</h1>
      </div>
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
