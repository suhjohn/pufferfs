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
    <main className="console-page">
      <header className="console-header">
        <div>
          <p className="eyebrow">organization</p>
          <h1>{orgQuery.data?.name ?? "organization"}</h1>
          <p className="muted">
            Review who can access shared roots and organization resources.
          </p>
        </div>
      </header>

      <section className="console-section" aria-label="organization members">
        <div className="section-heading">
          <div>
            <h2>members</h2>
            <p>Current users and their organization roles.</p>
          </div>
        </div>
      {membersQuery.isPending && <p className="muted">loading</p>}
      {membersQuery.data && (
          <div className="data-list">
            <div className="data-row data-row-head">
              <span>email</span>
              <span>role</span>
            </div>
          {membersQuery.data.map((m) => (
              <div key={m.userId} className="data-row">
                <strong>{m.email}</strong>
                <span className="tag">{m.role}</span>
            </div>
          ))}
        </div>
      )}
      </section>
    </main>
  );
}
