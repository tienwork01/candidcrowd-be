# Better Auth + Gin Authentication Implementation Plan

## Status and decision

**Status:** local email/password authentication is implemented and verified end to end. QA domain, transactional-email, Google, and Apple credentials remain deployment inputs.

Better Auth runs inside the Next.js frontend application. Gin remains the CandidCrowd product API and resource server. No domain or production URL is hard-coded: all origins, callback URLs, JWT issuer, audience, and JWKS URL are environment configuration.

This plan applies to:

- Frontend: `/home/acer/Desktop/work/candid-cowd-fe`
- Backend: `/home/acer/Desktop/work/candid-crowd-be`

## Current implementation snapshot

| Work item | Status |
| --- | --- |
| Better Auth, PostgreSQL and JWT dependencies installed in FE | Done |
| Better Auth server config and Next.js catch-all route | Done |
| Better Auth browser client and JWT API client | Done |
| Environment-driven URL, issuer, audience and trusted origins | Done |
| Gin EdDSA/JWKS validation and typed identity | Done |
| Gin CORS allowlist | Done |
| Product user profile/consent migration | Done and exercised against PostgreSQL |
| Better Auth schema migration | Done; run `pnpm migrate:auth` per environment |
| Login/register form integration | Done |
| Email verification/reset integration | Done; Mailpit locally, SMTP in QA |
| Gin `/me` and consent endpoints | Done |
| Event creation auth/consent gate | Done |
| FE event API integration | Done |
| Automated auth integration/E2E tests | Done |

External inputs needed only for QA provider testing—not for completing local email/password integration:

- transactional email provider credentials;
- Google/Apple OAuth credentials;
- QA frontend/API origins supplied through environment variables.

## Final delivery sequence

The implementation was completed in this order. Each stage was verified before it was marked done.

1. **Database bootstrapping:** create PostgreSQL `auth` schema, add a repeatable operator command, generate/apply Better Auth migrations, and verify `user`, `session`, `account`, `verification`, and `jwks` tables.
2. **Backend product identity:** implement `internal/profile`, `GET /api/v1/me`, `POST /api/v1/me/consents`, lazy local-user provisioning, profile synchronization, and required-consent checks.
3. **Backend authorization gate:** require verified email and current consent before creating events; preserve UUID-based resource ownership on list/get/create.
4. **Frontend email/password auth:** connect login/register forms, pending/error states, sanitized redirects, consent submission, and logout.
5. **Frontend verification/recovery:** replace the OTP preview with Better Auth email-link UX; connect resend verification, forgot password, and reset password.
6. **Frontend product API:** replace event localStorage persistence with authenticated Gin requests and handle `401`, `403`, unverified-email, and missing-consent states.
7. **QA providers:** configuration is complete; Google/Apple and real mail are enabled only when credentials are injected, and provider buttons remain hidden while unavailable.
8. **Verification:** run Go format/vet/test/build/lint, FE lint/typecheck/build, database migration smoke tests, and Playwright auth E2E.

The final release gate is one passing flow:

```text
register → consent → verify email → login/session → obtain short-lived JWT
→ Gin /me → create event → refresh/list event → logout → protected API denied
```

## Goals

1. Hosts can register, sign in, sign out, recover passwords, verify email, and later use Google/Apple.
2. Better Auth is the sole owner of credentials, host sessions, OAuth, and account recovery.
3. Gin authorizes product requests using short-lived, Better Auth-signed JWTs verified through JWKS.
4. CandidCrowd product tables use local UUIDs, never Better Auth IDs as foreign keys.
5. Guests remain anonymous and use only event-scoped `GuestSession` credentials.

## Non-goals

- No separate Go login, registration, password-reset, session, or OAuth implementation.
- No frontend storage of passwords, Better Auth session cookies, or API JWTs in localStorage/sessionStorage.
- No guest Better Auth accounts.
- No hard-coded `localhost`, QA, staging, or production domains in source code.

## Target request flow

```text
Browser
  │
  ├─ Next.js /api/auth/*
  │     └─ Better Auth validates credentials/OAuth and owns HttpOnly session cookies
  │
  ├─ authClient.token()
  │     └─ Better Auth JWT plugin creates a short-lived JWT
  │
  └─ Authorization: Bearer <JWT>
        │
        ▼
      Gin API
        ├─ fetch/cache Better Auth JWKS public keys
        ├─ validate token signature, alg, iss, aud, exp, nbf, sub
        ├─ resolve local CandidCrowd User from Better Auth subject
        └─ enforce resource ownership and product permissions
```

## Environment contract

### Frontend `.env.example`

```dotenv
# Browser-safe API origin; no trailing slash.
NEXT_PUBLIC_API_BASE_URL=http://localhost:8080

# Server-only Better Auth configuration.
BETTER_AUTH_SECRET=replace-with-a-long-random-secret
BETTER_AUTH_URL=http://localhost:3000
BETTER_AUTH_TRUSTED_ORIGINS=http://localhost:3000

# JWT resource-server contract. Use the same values in Gin.
BETTER_AUTH_JWT_ISSUER=http://localhost:3000
BETTER_AUTH_JWT_AUDIENCE=candidcrowd-api
BETTER_AUTH_JWT_EXPIRATION=10m

# Optional providers; unset means provider is disabled.
GOOGLE_CLIENT_ID=
GOOGLE_CLIENT_SECRET=
APPLE_CLIENT_ID=
APPLE_CLIENT_SECRET=

# Email delivery is configured before enabling email verification/reset in QA.
EMAIL_FROM=
```

### Backend `.env.example`

```dotenv
# Must match the frontend Better Auth values in each environment.
BETTER_AUTH_JWKS_URL=http://localhost:3000/api/auth/jwks
BETTER_AUTH_ISSUER=http://localhost:3000
BETTER_AUTH_AUDIENCE=candidcrowd-api

# Comma-separated browser origins allowed to call Gin.
CORS_ALLOWED_ORIGINS=http://localhost:3000
```

### QA deployment rule

The QA deploy pipeline injects all values from its secret/environment store. It sets:

```text
BETTER_AUTH_URL             = QA frontend origin
BETTER_AUTH_TRUSTED_ORIGINS = QA frontend origin[, preview origins]
BETTER_AUTH_JWT_ISSUER      = QA frontend origin
BETTER_AUTH_JWKS_URL        = QA frontend origin + /api/auth/jwks
NEXT_PUBLIC_API_BASE_URL    = QA Gin origin
CORS_ALLOWED_ORIGINS        = QA frontend origin
```

`BETTER_AUTH_SECRET`, OAuth secrets, SMTP credentials, database URLs, and Redis passwords are server secrets and must never receive an `NEXT_PUBLIC_` prefix.

## Implementation phases

### Phase 1 — Better Auth foundation in Next.js

1. Install `better-auth` in the frontend repository.
2. Create `src/lib/auth.ts` with:
   - email/password auth;
   - JWT plugin;
   - `issuer`, `audience`, and 10-minute expiry from environment;
   - explicit `EdDSA`/Ed25519 key pair configuration or a consciously selected alternative;
   - monthly key rotation and 30-day grace period;
   - `trustedOrigins` parsed from environment;
   - secure cookie behavior based on environment.
3. Create `src/lib/auth-client.ts` with `createAuthClient` and `jwtClient`.
4. Add `src/app/api/auth/[...all]/route.ts` exposing Better Auth handlers.
5. Create and apply Better Auth database migrations/tables. Better Auth auth tables must be separate from CandidCrowd product models. A PostgreSQL schema such as `auth` is preferred if the selected Better Auth adapter supports it; otherwise use unambiguous table names.
6. Add Google/Apple configuration only when QA callback origins and provider credentials are ready.

**Acceptance:** a local host can register and log in through Better Auth; an authenticated browser can obtain a JWT from `authClient.token()`.

### Phase 2 — Gin JWT resource-server hardening

1. Update `internal/platform/auth`:
   - accept only the configured Better Auth signing algorithm; Better Auth JWT defaults to `EdDSA`, so remove the current RS/ES-only assumption unless Better Auth is explicitly configured to use ES256;
   - validate `iss`, `aud`, `exp`, `nbf`, `sub`, signature, and allowed algorithm;
   - cache JWKS safely and refresh on an unknown `kid`;
   - never log JWTs or authorization headers;
   - return stable `401` error codes for absent, expired, malformed, or invalid tokens.
2. Add `internal/platform/cors` or a focused Gin CORS configuration. It must allow only configured origins, methods, and headers (`Authorization`, `Content-Type`, `X-Request-ID`); never use `*` for authenticated endpoints.
3. Add an `IdentityResolver` in the auth boundary. It maps JWT identity to a CandidCrowd local user, without exposing Better Auth concepts to handlers/services.
4. Make `auth.Identity` carry only needed, verified values:

   ```go
   type Identity struct {
       BetterAuthUserID string
       Email            string
       Name             string
       EmailVerified    bool
   }
   ```

5. Add `GET /api/v1/me` as the first protected contract. It proves frontend token exchange, Gin verification, and local-user provisioning.

**Acceptance:** a valid JWT reaches a protected Gin endpoint; invalid issuer/audience/signature/expiry never does.

### Phase 3 — CandidCrowd user and consent data

1. Add an application migration for product metadata:

   ```text
   users.display_name
   users.email_verified_at
   users.last_authenticated_at
   user_consents
     - user_id
     - document_type
     - document_version
     - accepted_at
   ```

2. On the first valid Gin request, upsert local `users` using `better_auth_user_id` as the immutable external identity reference.
3. Update the local cached email/name/verification timestamp from verified JWT claims. Better Auth remains source of truth.
4. Add `POST /api/v1/me/consents`. Event creation requires current Terms and Privacy consent.
5. Do not create a second password, login method, session table, or social-account table in Go.

**Acceptance:** `events.host_id` references the internal CandidCrowd UUID, while one local user maps to one Better Auth subject.

### Phase 4 — Wire the frontend UI to real auth

1. Replace UI-only submit handlers in `features/auth/components/auth-form.tsx`:
   - Register: Better Auth email signup; then submit product consent; redirect to host dashboard/create event.
   - Login: Better Auth email sign-in; redirect to safe relative `next` path.
   - Google/Apple: Better Auth social sign-in with a fixed, allowlisted callback path.
2. Replace `/create` localStorage draft with an authenticated host route and `POST /api/v1/events` via a shared `src/lib/api-client.ts`.
3. `api-client` calls `authClient.token()` immediately before a protected request and sends the JWT in memory only.
4. On Gin `401`, attempt one fresh token retrieval; if absent/invalid, sign out locally and redirect to `/login?next=<relative-path>`.
5. On Gin `403`, show an authorization error without logging the user out.
6. Keep guest public upload routes unauthenticated and separate from the host API client.

**Acceptance:** an authenticated host can create an event after browser refresh, and a signed-out browser cannot call host endpoints.

### Phase 5 — email verification and account recovery

1. Configure a transactional email provider for QA before enabling outbound mail.
2. Use Better Auth’s email-verification and reset-password flows.
3. Adapt the current frontend OTP preview to Better Auth’s supported verification-link UX unless a product requirement explicitly requires one-time codes.
4. Do not disclose whether an email address exists during password recovery.
5. Ensure reset and verification callback destinations are trusted/allowlisted origins only.

**Acceptance:** reset and verification flows complete without Go seeing passwords, reset tokens, or verification secrets.

### Phase 6 — QA hardening and release readiness

1. Add rate limiting at Better Auth entrypoints and Gin host/guest routes independently.
2. Set production cookie security and HTTPS-only behavior.
3. Confirm OAuth provider callback URLs for QA before enabling providers.
4. Add monitoring for failed JWT verification, JWKS refresh failure, sign-in failure, and provider callback failure; exclude credentials and PII from logs.
5. Maintain a runbook for Better Auth key rotation, secret rotation, OAuth secret rotation, and outage behavior.

## Test plan

### Frontend

- Email/password registration and sign-in success/failure.
- Redirect to requested safe internal page after login.
- Logout clears access to dashboard and Gin calls.
- No password/JWT in browser localStorage or sessionStorage.
- Google/Apple callback error and cancellation states.
- Verification and reset journeys in QA mail sandbox.

### Backend

- Valid JWT with configured `EdDSA` algorithm succeeds.
- Expired, invalid-signature, wrong `kid`, wrong issuer, wrong audience, missing subject, and unsupported-algorithm JWTs return `401`.
- JWKS cache refreshes after key rotation.
- First authenticated request creates/updates local product user.
- User A cannot read or mutate User B’s event/media.
- Guest session token cannot access host endpoints; host JWT cannot substitute for guest session token.
- CORS rejects untrusted origins.

### End-to-end QA

```text
Register → accept consent → verify email → create event → refresh page
→ obtain JWT → call Gin → sign out → confirm Gin access expires/fails.
```

## Security decisions

- Browser session cookie: Better Auth owned, `HttpOnly`, `Secure` outside local development, `SameSite=Lax` unless a documented flow requires otherwise.
- API JWT: 10-minute lifetime, in-memory only, supplied in `Authorization` header.
- Token handling: Gin verifies a signed JWT; it does not decrypt a secret token or share Better Auth’s private key.
- Key material: Better Auth private keys remain in Better Auth storage; Gin reads only public JWKS.
- Revocation: after logout, a minted JWT can remain valid until expiry. Ten minutes is the MVP maximum exposure. If immediate revocation becomes required, add a centralized Better Auth session-introspection/revocation adapter in `internal/platform/auth` rather than distributing checks across handlers.

## Required changes to the current backend scaffold

1. Change `internal/platform/auth` from RS256/ES256-only validation to the explicit Better Auth JWT algorithm selected in Phase 1.
2. Add CORS configuration and `GET /api/v1/me`.
3. Replace bare `ExternalID` naming with `BetterAuthUserID` where that makes data provenance clearer.
4. Add user profile/consent migration and service.
5. Add JWT verification integration tests with a controllable JWKS server.

## Completion criteria

The plan is complete when:

- QA origin values are injected solely through environment configuration.
- Better Auth owns host credentials/sessions and Gin owns only product authorization.
- No auth secret, session cookie, password, JWT, provider secret, or private JWK appears in browser storage, logs, migrations, or source control.
- Host APIs accept only valid Better Auth JWTs and enforce CandidCrowd resource ownership.
- Guest upload remains account-free.

## Integration execution plan

This is the ordered implementation backlog for turning the existing scaffolding into a working FE-to-BE authentication flow. Complete each step before beginning the next one.

### I1 — Make Better Auth runnable locally

**Frontend work**

1. Keep `src/lib/auth.ts`, `src/lib/auth-client.ts`, and `src/app/api/auth/[...all]/route.ts` as the only Better Auth entry points.
2. Create an `auth` PostgreSQL schema and grant the application database user access to it.
3. Run `pnpm exec auth migrate --yes` against `BETTER_AUTH_DATABASE_URL` after the schema exists.
4. Add a `migrate:auth` script that calls the Better Auth CLI; it is an operator/deployment command, not an application-startup action.
5. Start Next.js with the local environment values and verify:

   ```text
   GET /api/auth/jwks             → returns public JWK set
   POST /api/auth/sign-up/email   → creates a Better Auth user/session
   GET /api/auth/get-session      → returns authenticated session
   ```

**Done when:** Better Auth tables exist only in the `auth` schema and a browser session works at the frontend origin.

### I2 — Connect frontend login and registration UI

**Files**

```text
src/features/auth/components/auth-form.tsx
src/features/auth/components/auth.css
```

**Registration submit flow**

1. Disable the form while pending; preserve native labels, password manager attributes, focus styles, and error status region.
2. Call `authClient.signUp.email({ name, email, password, callbackURL })`.
3. On success, call `POST /api/v1/me/consents` through `apiFetch` with Terms/Privacy version values.
4. Redirect to `/verify-email` while the host has not verified email; redirect to `/create` only when verified.
5. Render Better Auth errors as a form-level safe message. Do not reveal raw server errors.

**Login submit flow**

1. Call `authClient.signIn.email({ email, password, callbackURL })`.
2. Read only a relative `next` URL. Reject values that do not start with a single `/`.
3. Redirect to the safe `next` path or `/create`.

**OAuth flow**

1. Wire Google/Apple buttons to `authClient.signIn.social({ provider, callbackURL })` only when credentials are configured in QA.
2. Keep unavailable provider buttons hidden—not disabled with a false success state—when that provider is not configured.

**Done when:** browser devtools shows Better Auth requests on form submit, no password or JWT appears in browser storage, and successful login reaches a protected host page.

### I3 — Replace OTP preview with Better Auth email-link UX

**Files**

```text
src/features/auth/components/account-recovery.tsx
src/app/(auth)/verify-email/page.tsx
tests/auth-recovery.spec.ts
```

1. Remove the six-digit OTP component and its preview-only completion state for `verify-email`.
2. Present an inbox-focused page: heading, explain that a verification link was sent, opening-mail CTA, resend action, change-email/login link, and a polite status region.
3. Send/resend through Better Auth’s verification-email client method; use a configured relative callback path.
4. The verification callback must obtain a valid Better Auth session and redirect to `/create` or the sanitized `next` destination.
5. Retain password reset as Better Auth reset-link flow. Do not implement reset OTP or custom password verification in Gin.

**Done when:** QA email sandbox receives a usable verification link, the link marks the Better Auth user verified, and the browser reaches the host flow.

### I4 — Complete Gin identity and user-profile API

**Backend work**

1. Add `GET /api/v1/me` behind Better Auth JWT middleware.
2. Add migration fields and consent records described in Phase 3.
3. Create a feature-focused `internal/profile` package rather than adding auth business logic to `httpapi`:

   ```text
   internal/profile/
   ├── profile.go
   ├── service.go
   ├── handler.go
   └── service_test.go
   ```

4. On authenticated `/me`, upsert local `users` by `better_auth_user_id` and update cached non-secret profile data from verified claims.
5. Add `POST /api/v1/me/consents`; accept versioned document types only.
6. Require `email_verified_at` and current consents before `POST /api/v1/events`.

**Contract**

```json
GET /api/v1/me
{
  "id": "candidcrowd-internal-uuid",
  "email": "host@example.com",
  "name": "Jamie Morgan",
  "email_verified": true,
  "has_required_consents": true
}
```

**Done when:** Gin never uses Better Auth IDs as event foreign keys, but can deterministically authorize every host request.

### I5 — Replace local event draft with live API

**Files**

```text
src/features/event/components/event-draft-form.tsx
src/lib/api-client.ts
src/app/create/page.tsx
```

1. Replace `localStorage` persistence with `apiFetch("/api/v1/events", { method: "POST", ... })`.
2. Map backend validation codes to accessible field/form errors.
3. On `401`, retrieve a fresh JWT once; redirect to `/login?next=/create` only if unavailable.
4. On an unverified/consent-blocked response, redirect to `/verify-email` or consent completion—not to login.
5. Render a real event slug/link only after backend creation succeeds.

**Done when:** create event survives page reload and appears in `GET /api/v1/events` for only the authenticated host.

### I6 — Cross-origin QA configuration

1. Deploy frontend and Gin to QA origins supplied by CI environment variables.
2. Set Better Auth `trustedOrigins` to frontend QA origin(s), never to a wildcard.
3. Set Gin `CORS_ALLOWED_ORIGINS` to the frontend QA origin(s).
4. Set Gin `BETTER_AUTH_JWKS_URL` to the frontend QA `/api/auth/jwks` endpoint.
5. Set matching JWT issuer/audience on both services.
6. Verify HTTPS and secure cookies in QA; do not test secure-cookie behavior only on localhost.

**Done when:** no source file contains QA hostnames and a deployed browser can complete signup, verification, event creation, refresh, and logout.

### I7 — End-to-end acceptance suite

Add Playwright tests using QA-safe seeded inbox/OAuth test accounts:

```text
new host → signup → consent → verification email → verification callback
→ create event → API event list → refresh → logout → host API returns 401
```

Gin integration & unit tests (Implemented & Verified):

```text
internal/platform/auth/auth_test.go:
  - valid EdDSA JWT → 200/204
  - expired JWT → 401
  - wrong issuer/audience → 401
  - missing bearer header → 401
  - rotated JWKS kid → refresh public keys then 200

internal/platform/cors/cors_test.go:
  - allowed origins → 200 with Access-Control-Allow-Origin & Vary headers
  - preflight OPTIONS request → 204 No Content
  - disallowed origin → 403 Forbidden
  - request without Origin header → passes through

internal/profile/service_test.go:
  - lazy user provisioning from Better Auth subject on first request
  - user profile synchronization (name, last_authenticated_at)
  - versioned consent auditing (terms & privacy versions, ErrInvalidConsent on mismatch)
  - RequireEventCreation gate checking email verification & required consents
  - HTTP handlers for GET /api/v1/me and POST /api/v1/me/consents (200 / 422)

internal/event/service_test.go:
  - event creation with UUID host assignment, slug, and status
  - event creation validation (empty name, negative guest count rejected)
  - multi-tenant isolation: host A cannot read host B's event (404/ErrRecordNotFound)
  - list events returns only caller's events
  - HTTP handler gating: blocks unverified email (403) and missing consent (403)
```

## QA & Production Deployment Notes

1. **Environment Variable Pairing:**
   - `BETTER_AUTH_URL` on FE must match `BETTER_AUTH_ISSUER` on BE.
   - `BETTER_AUTH_URL + "/api/auth/jwks"` must be supplied as `BETTER_AUTH_JWKS_URL` to BE.
   - `BETTER_AUTH_JWT_AUDIENCE` and `BETTER_AUTH_AUDIENCE` must both be `candidcrowd-api`.
   - `BETTER_AUTH_TRUSTED_ORIGINS` and `CORS_ALLOWED_ORIGINS` must contain the exact frontend origins without trailing slashes.
   - `BETTER_AUTH_DATABASE_URL` must specify `?options=-c%20search_path%3Dauth` so Better Auth stays isolated in the `auth` schema.

2. **Transactional Email:**
   - Locally uses Mailpit (SMTP port 1025/1026).
   - In QA/Prod, configure SMTP provider (`SMTP_HOST`, `SMTP_PORT`, `SMTP_USER`, `SMTP_PASSWORD`, `SMTP_SECURE`, `EMAIL_FROM`) to deliver verification and reset links.

3. **OAuth Providers:**
   - Google & Apple OAuth buttons are hidden when `NEXT_PUBLIC_GOOGLE_AUTH_ENABLED` / `NEXT_PUBLIC_APPLE_AUTH_ENABLED` are false.
   - For QA, register redirect URIs: `https://<qa-fe-origin>/api/auth/callback/google` and `https://<qa-fe-origin>/api/auth/callback/apple`.

4. **Proactive Auth Gating:**
   - FE `/create` page proactively redirects unverified hosts to `/verify-email?email=<encoded-email>` on server render, preventing draft loss.

**Release gate:** all unit/integration test suites pass, frontend has no credential/token storage, and backend logs contain no Authorization header, session, password, or JWT payload.

