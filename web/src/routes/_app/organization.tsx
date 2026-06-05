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
      <div className="page-heading">
        <h1>{orgQuery.data?.name ?? "organization"}</h1>
        <h2>members</h2>
      </div>
      {membersQuery.isPending && <p className="muted">loading</p>}
      {membersQuery.data && (
        <div className="list">
          {membersQuery.data.map((m) => (
            <div key={m.userId} className="list-item">
              <div className="row">
                <strong>{m.email}</strong>
                <span className="tag">{m.role}</span>
              </div>
            </div>
          ))}
        </div>
      )}
    </main>
  );
}
