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

**503 Service Unavailable.** A dependency, such as the database, is unreachable. Retry later.
