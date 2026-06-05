import { createFileRoute } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { fetchMembers, fetchOrg } from "../../lib/queries";

export const Route = createFileRoute("/_app/organization")({
  component: Organization,
});

function Organization() {
  const orgQuery = useQuery({ queryKey: ["org"], queryFn: fetchOrg });
  const membersQuery = useQuery({
    queryKey: ["members"],
    queryFn: fetchMembers,
  });

  return (
    <main className="container">
      <h1 className="prompt">{orgQuery.data?.name ?? "organization"}</h1>
      <h2>members</h2>
      {membersQuery.isPending && <p className="muted">// loading…</p>}
      {membersQuery.data?.map((m) => (
        <div key={m.userId} className="card">
          <div className="row">
            <strong>{m.email}</strong>
            <span className="tag">{m.role}</span>
          </div>
        </div>
      ))}
    </main>
  );
}
