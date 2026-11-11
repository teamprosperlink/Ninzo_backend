# 0015: Console sign-in, roles and approvals

- Date: 2026-11-11
- Status: accepted (the owner's answers in decision 0014, with the details below decided by the backend)

## Context
Decision 0014 settled that console staff sign in with an ID and password plus TOTP, that the roles are admin, ops, support and hub manager, and that a refund above a set amount and any settings change need approval. The ops console KT doc proposed a browser cookie session and six roles.

## Decision
- **A separate console API** (`apps/console-api`, `contracts/console/openapi.yaml`), with bearer tokens like the other three APIs, not a cookie. The console is a web app; it keeps the access token in memory and the refresh token in a secure store, as the KT doc's frontend notes allow.
- **Two steps, always.** A password alone never gives a session: `consoleLogin` returns a short-lived challenge, and `verifyTotp` starts the session. A first sign-in sets up TOTP with `enrollTotp`. A temporary password must be changed before anything else.
- **Four roles** (0014), checked on the server for every call, with 403 naming the missing permission: admin (users, settings, flags, approvals, audit, everything below); ops (zones and the kill switch to request, hubs and staff, riders, menu, gates, reviews); support (reviews, support alerts; refunds and tickets in v0.10.0); hub_manager (their own hub and its staff).
- **Approvals.** A settings change, a kill-switch change and a flag change are requested (202, pending) and applied only when a second person with the admin role approves. The requester cannot approve their own. A refund above the set amount joins in v0.10.0.
- **Lockout.** Five failed sign-ins in 15 minutes lock the account for 15 minutes, so a stranger cannot lock admins out for good. A wrong ID or password always gets the same answer; only the right password learns the account is locked. An admin reset clears a lock, and a command-line tool on the server creates the first admin and unlocks one (part B).
- **A temporary password** must be changed first: until then every call but changePassword, getConsoleUser, refreshToken and signOut is 403 password_change_required.
- **Permissions.** Each operation names its permission (`x-permission` in the contract, the Permission enum, and in the 403's missing_permission). Each role's list is in the Permission schema's description; getConsoleUser returns the signed-in user's.
- **Approvals carry their change.** An approval shows the change itself (typed by kind), who asked and why, who decided and why, and, if it could not be applied, failure_code. A settings change names the version it was based on; if the settings moved on, approving fails it as stale_version and nothing changes. The requester can cancel a pending approval; it lapses after 24 hours.

## Consequences
- The console developer builds against `/console/v1` on the mock now.
- Console staff are a new kind of user in identity, with their own sessions.

## Update, 2026-11-11 (#152)
Owner: console staff sign in with an **email address** and password, then the authenticator code. The field is still `login_id`, so the contract does not break; it accepts an email address as well as the older ID form, so users already made keep working (migration 0038).

Later the same day the owner changed it: **email and password now; the authenticator code comes before launch.** A switch, `CONSOLE_TOTP`, is `optional` on staging: a user with no authenticator signs in with the password alone, and one who has set one up is still asked for it. The default is `required`, and production refuses `optional`, so turning it to required is a go-live step (v1.0.0).
