# API problem types

Every error from `/api/v1` is an [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) problem
details object sent as `application/problem+json` ([ADR-0005](../adr/0005-api-auth-and-read-model.md) §1):

```json
{
  "type": "https://github.com/aljazkovac/pulse-pipeline/blob/main/docs/api/problems.md#not-found",
  "title": "Not found",
  "status": 404,
  "detail": "No such API endpoint."
}
```

Clients branch on `type`, which is this page's URL plus the section anchor below. `title` and
`status` are fixed per type. `detail`, when present, explains the occurrence in words written for
the client: it never carries upstream (provider) or internal (database, Go) error text, which is
logged instead. Error responses carry `Cache-Control: no-store` like every other `/api` response.

New types are added here together with the code that returns them; a Go test fails when a type
in `backend/internal/api` has no section on this page. Never change an existing type's anchor.

## invalid-request

**400 Bad Request.** A path or query parameter, header or body is missing or malformed. `detail`
names the parameter when known.

## not-found

**404 Not Found.** No API endpoint matches the path, or the requested resource does not exist
for the signed-in user. Resources of other users are always reported as not found.

## method-not-allowed

**405 Method Not Allowed.** The path exists but not for this method. The `Allow` header lists the
methods it accepts.

## internal

**500 Internal Server Error.** An unexpected server-side failure. The details are in the API's
logs; retrying may help.

## unavailable

**503 Service Unavailable.** A dependency, such as the database or Google's sign-in service, is
unreachable; retry later. Google sign-in also answers with this type when the server has no Google
OAuth client configured, which retrying does not fix.

## unauthenticated

**401 Unauthorized.** The operation needs a session and the request has none, or its session is
unknown, signed out or expired (30 days idle or 90 days after sign-in). Sign in again with
`GET /api/v1/auth/google/login`. When the request carried a dead session cookie, the response also
expires it.

## sign-in-failed

**400 Bad Request.** Google sign-in could not be completed: the flow cookie is missing or expired
(sign-in takes at most ten minutes), `state` does not match the browser that started the sign-in,
the user cancelled at Google, or Google did not accept the authorization code. Start the sign-in
again.

## sign-in-denied

**403 Forbidden.** Google confirmed the account, but it may not sign in: its email address is not
in `PULSE_ALLOWED_EMAILS`, Google has not verified it, Google is not authoritative for it (a
non-Gmail, non-Workspace address), or it belongs to an existing Pulse user that this Google account
is not bound to. `detail` says which; the last two need the operator
(`pulse-worker bind-google-identity`). No session is created and no account changes.

## cross-origin-request

**403 Forbidden.** A state-changing request (`POST`, `PUT`, `PATCH`, `DELETE`) came from another
origin, as reported by `Sec-Fetch-Site` or, in older browsers, `Origin`. The web app and the API
share an origin, so the app never sees this; it stops cross-site request forgery.
