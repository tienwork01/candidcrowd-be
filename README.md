# CandidCrowd API

Production-oriented Go/Gin modular monolith for CandidCrowd's host event management and anonymous, direct-to-R2 guest upload flow.

The enduring directory and dependency conventions are defined in [Project Structure Design System](docs/architecture/project-structure.md).

## Architecture

`cmd/api` is deliberately thin: it reads environment configuration, creates dependencies, and owns process lifecycle. Product code lives under `internal/` and is grouped by feature (`event`, `guest`, `media`, `profile`, `user`). Platform integration code is isolated under `internal/platform`.

Handlers bind and validate HTTP input, then call a feature service with the request context. Services own product rules and use GORM only where persistence is genuinely needed; there is no generic repository layer. `media.Service` depends on a small R2 storage interface to make object-store behavior testable without making the domain R2-specific.

Better Auth remains the source of truth for host authentication. Its JWT/JWKS validation is contained in `internal/platform/auth`; middleware adds a typed `auth.Identity` to the Gin context. `profile.Service` lazily maps that identity to a local user and records versioned consent. The local `users.id` UUID is the foreign key in product tables and `better_auth_user_id` is only an external identity reference. Guests instead receive an opaque, hashed, event-scoped `GuestSession` token.

## Local development

1. Copy `.env.example` to `.env` and set the local credentials.
2. Start PostgreSQL, Redis, and Mailpit with `docker compose up -d postgres redis mailpit`.
3. Install [golang-migrate](https://github.com/golang-migrate/migrate/tree/master/cmd/migrate) and run `make migrate-up` with `DATABASE_URL` exported. Migration `000003` creates the isolated PostgreSQL `auth` schema.
4. In the Next.js repository, copy its `.env.example`, use a `BETTER_AUTH_DATABASE_URL` whose `search_path` is `auth`, then run `pnpm migrate:auth`.
5. Start Next.js, then start this API with `make run`. Mailpit's local inbox is available at `http://localhost:8025`.

For `make run`, keep `POSTGRES_PASSWORD` and the password embedded in `DATABASE_URL` identical; do the same for `REDIS_PASSWORD` and `REDIS_URL`. Compose overrides those URLs with its internal `postgres` and `redis` service DNS names. PostgreSQL and Redis bind to `127.0.0.1` by default, while the API binds to all interfaces. Set `API_BIND_ADDRESS=127.0.0.1` when a reverse proxy is on the same host.

The API listens at `HTTP_ADDR` (default `:8080`). `GET /healthz` reports process health; `GET /readyz` checks PostgreSQL and Redis.

## API

Host endpoints require `Authorization: Bearer <Better Auth JWT>`:

- `GET /api/v1/me`
- `POST /api/v1/me/consents`
- `POST /api/v1/events`
- `GET /api/v1/events`
- `GET /api/v1/events/:id`

Public guest endpoints:

- `GET /api/v1/public/events/:slug`
- `POST /api/v1/public/events/:slug/sessions`
- `POST /api/v1/public/events/:slug/uploads`
- `POST /api/v1/public/events/:slug/uploads/:uploadId/complete`

Create-upload validates the active event, opaque guest session, allow-listed image/video MIME family, size, per-event quota, and Redis rate limit. It creates a `pending` media row and returns a short-lived presigned R2 PUT URL. The browser uploads directly to the private bucket. Complete-upload uses R2 `HeadObject` to verify expected size and content type before marking the row `ready` and charging quota. Client filenames are never object keys.

## Database and migrations

Migrations in `migrations/` are the only product-schema mechanism; the application never calls GORM `AutoMigrate`. `000001` creates the product tables, `000002` adds the host profile and consent audit, and `000003` creates the isolated Better Auth schema. Better Auth owns its tables inside that schema and its pinned CLI applies them with `pnpm migrate:auth` from the frontend repository.

Redis currently holds upload rate-limit state. A background worker can be added later as a separate process under `cmd/worker`; its queue transport belongs in `internal/platform`, while job-specific rules remain in their feature package. For production, use separate Redis instances or at least separate credentials/policies for cache/rate limiting and durable queues.

## Adding a feature

Add a feature-focused package under `internal/<feature>` with its model, service, handler, and colocated `_test.go` files. Wire concrete dependencies only in `cmd/api/main.go`, and mount routes in `internal/httpapi/router.go`. Keep external SDKs under `internal/platform`; inject a narrow interface only where a service needs a replaceable external capability. Add a forward and rollback migration before using new persisted data.

## Quality commands

```sh
make fmt
make vet
make test
make build
make lint
```

`docker-compose.test.yml` provisions disposable PostgreSQL, Redis, and LocalStack S3 endpoints for black-box/integration tests. Keep integration tests separate from fast colocated unit tests; production credentials must never be used in test configuration.
