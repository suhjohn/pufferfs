import { createFileRoute } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { capture } from "../../lib/analytics";
import {
  deleteOrgInvite,
  fetchMembers,
  fetchOrg,
  fetchOrgInvites,
  inviteOrgMember,
  removeOrgMember,
  updateMemberRole,
} from "../../lib/queries";

export const Route = createFileRoute("/_app/organization")({
  component: Organization,
});

function Organization() {
  const { session } = Route.useRouteContext();
  const queryClient = useQueryClient();
  const trackedView = useRef(false);
  const [inviteEmail, setInviteEmail] = useState("");
  const [inviteRole, setInviteRole] = useState(defaultInviteRole(session.role));
  const orgQuery = useQuery({ queryKey: ["org"], queryFn: fetchOrg });
  const membersQuery = useQuery({
    queryKey: ["members"],
    queryFn: fetchMembers,
  });
  const invitesQuery = useQuery({
    queryKey: ["org-invites"],
    queryFn: fetchOrgInvites,
  });
  const inviteMember = useMutation({
    mutationFn: inviteOrgMember,
    onSuccess: (invite) => {
      capture("invite_created", {
        role: invite.role,
        email_domain: emailDomain(invite.email),
      });
      setInviteEmail("");
      queryClient.invalidateQueries({ queryKey: ["org-invites"] });
    },
  });
  const changeRole = useMutation({
    mutationFn: updateMemberRole,
    onSuccess: (member) => {
      capture("org_member_role_updated", {
        role: member.role,
      });
      queryClient.invalidateQueries({ queryKey: ["members"] });
    },
  });
  const removeMember = useMutation({
    mutationFn: removeOrgMember,
    onSuccess: () => {
      capture("org_member_removed");
      queryClient.invalidateQueries({ queryKey: ["members"] });
    },
  });
  const revokeInvite = useMutation({
    mutationFn: deleteOrgInvite,
    onSuccess: () => {
      capture("invite_revoked");
      queryClient.invalidateQueries({ queryKey: ["org-invites"] });
    },
  });

  const manageableRoles = assignableRoles(session.role);
  const canManageOrg = manageableRoles.length > 0;

  useEffect(() => {
    if (trackedView.current || !membersQuery.data || !invitesQuery.data) return;
    trackedView.current = true;
    capture("organization_viewed", {
      member_count: membersQuery.data.length,
      pending_invite_count: invitesQuery.data.length,
      can_manage_org: canManageOrg,
    });
  }, [membersQuery.data, invitesQuery.data, canManageOrg]);

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

      {canManageOrg && (
        <section className="console-section" aria-label="invite member">
          <div className="section-heading">
            <div>
              <h2>invite</h2>
              <p>Invite a teammate by email and choose their starting role.</p>
            </div>
          </div>
          <form
            className="settings-grid"
            onSubmit={(event) => {
              event.preventDefault();
              inviteMember.mutate({ email: inviteEmail, role: inviteRole });
            }}
          >
            <label className="field-label">
              <span>email</span>
              <input
                type="email"
                value={inviteEmail}
                onChange={(event) => setInviteEmail(event.target.value)}
                placeholder="teammate@example.com"
                required
              />
            </label>
            <label className="field-label">
              <span>role</span>
              <select
                value={inviteRole}
                onChange={(event) => setInviteRole(event.target.value)}
              >
                {manageableRoles.map((role) => (
                  <option key={role} value={role}>
                    {role}
                  </option>
                ))}
              </select>
            </label>
            <button
              className="btn btn-sm"
              disabled={inviteMember.isPending || !inviteEmail.trim()}
              type="submit"
            >
              {inviteMember.isPending ? "inviting" : "invite"}
            </button>
          </form>
          {inviteMember.isError && (
            <p className="muted">error: {(inviteMember.error as Error).message}</p>
          )}
        </section>
      )}

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
            <div className="data-row data-row-head member-data-row">
              <span>email</span>
              <span>role</span>
              <span>joined</span>
              <span />
            </div>
            {membersQuery.data.map((m) => {
              const canChange =
                canManageOrg &&
                m.user_id !== session.userId &&
                canManageRole(session.role, m.role);
              return (
              <div key={m.user_id} className="data-row member-data-row">
                <strong>{m.email}</strong>
                {canChange ? (
                  <select
                    value={m.role}
                    onChange={(event) =>
                      changeRole.mutate({
                        userId: m.user_id,
                        role: event.target.value,
                      })
                    }
                    disabled={changeRole.isPending}
                  >
                    {manageableRoles.map((role) => (
                      <option key={role} value={role}>
                        {role}
                      </option>
                    ))}
                  </select>
                ) : (
                  <span className="tag">{m.role}</span>
                )}
                <span className="muted">{formatDateTime(m.joined_at)}</span>
                {canChange ? (
                  <button
                    className="btn btn-sm btn-danger"
                    disabled={removeMember.isPending}
                    onClick={() => removeMember.mutate(m.user_id)}
                  >
                    remove
                  </button>
                ) : (
                  <span />
                )}
              </div>
              );
            })}
          </div>
        )}
      </section>

      <section className="console-section" aria-label="pending invites">
        <div className="section-heading">
          <div>
            <h2>pending invites</h2>
            <p>People who can join this organization the next time they sign in.</p>
          </div>
        </div>
        {invitesQuery.isPending && <p className="muted">loading</p>}
        {invitesQuery.data && invitesQuery.data.length === 0 && (
          <div className="empty-state">no pending invites</div>
        )}
        {invitesQuery.data && invitesQuery.data.length > 0 && (
          <div className="data-list">
            <div className="data-row data-row-head invite-data-row">
              <span>email</span>
              <span>role</span>
              <span>created</span>
              <span />
            </div>
            {invitesQuery.data.map((invite) => (
              <div key={invite.id} className="data-row invite-data-row">
                <strong>{invite.email}</strong>
                <span className="tag">{invite.role}</span>
                <span className="muted">{formatDateTime(invite.created_at)}</span>
                {canManageOrg && canManageRole(session.role, invite.role) ? (
                  <button
                    className="btn btn-sm btn-danger"
                    disabled={revokeInvite.isPending}
                    onClick={() => revokeInvite.mutate(invite.id)}
                  >
                    revoke
                  </button>
                ) : (
                  <span />
                )}
              </div>
            ))}
          </div>
        )}
      </section>
    </main>
  );
}

function assignableRoles(role: string) {
  if (role === "owner") return ["owner", "admin", "editor", "viewer"];
  if (role === "admin") return ["editor", "viewer"];
  return [];
}

function defaultInviteRole(role: string) {
  return role === "owner" ? "admin" : "viewer";
}

function canManageRole(actorRole: string, targetRole: string) {
  if (actorRole === "owner") return true;
  if (actorRole === "admin") return targetRole === "editor" || targetRole === "viewer";
  return false;
}

function formatDateTime(value?: string) {
  if (!value) return "unknown";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "unknown";
  return date.toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
  });
}

function emailDomain(email: string) {
  const [, domain] = email.split("@");
  return domain || "unknown";
}
