# Project Structure Design System

This document is the structural contract for the CandidCrowd Go API. It keeps the project an idiomatic Gin modular monolith: simple to navigate now and safe to extend as event and upload capabilities grow.

## Canonical layout

```text
.
├── cmd/
│   └── api/                     # executable entrypoint only
│       └── main.go              # configuration, dependency wiring, process lifecycle
├── internal/                    # application-private packages
│   ├── apierror/                # canonical API error representation/response
│   ├── config/                  # environment configuration
│   ├── httpapi/                 # Gin router, cross-feature HTTP composition
│   ├── platform/                # infrastructure adapters
│   │   ├── auth/                # Better Auth token/JWKS validation middleware
│   │   ├── cors/                # configured browser-origin boundary
│   │   ├── database/            # PostgreSQL/GORM connection setup
│   │   ├── r2/                  # Cloudflare R2 S3-compatible adapter
│   │   └── redis/               # Redis ephemeral-state/rate-limit adapter
│   ├── user/                    # local user identity model
│   ├── profile/                 # identity sync and versioned host consent
│   ├── event/                   # event model, host service, host handlers
│   ├── guest/                   # anonymous event-scoped sessions
│   └── media/                   # upload authorization and completion rules
├── migrations/                  # ordered golang-migrate SQL pairs
├── docs/architecture/           # enduring architecture decisions
├── Dockerfile
├── docker-compose.yml
├── docker-compose.test.yml
├── Makefile
├── go.mod
└── go.sum
```

## Ownership and dependency rules

| Area | Owns | May depend on |
| --- | --- | --- |
| `cmd/api` | process bootstrap and concrete dependency wiring | every internal package |
| `internal/httpapi` | Gin router and transport composition | feature handlers, `apierror`, platform middleware |
| `internal/<feature>` | one business capability, its models/services/handlers | sibling feature only when it represents an explicit business relationship; narrow platform interfaces |
| `internal/platform` | SDK clients and external-system behavior | external SDKs, config |
| `internal/apierror` | stable JSON error contract | Gin only |
| `migrations` | durable schema changes | PostgreSQL SQL only |

Rules:

1. `cmd/` must not contain business rules, GORM queries, or Gin handlers.
2. A feature keeps its model, service, handler, and `_test.go` files together until real complexity makes a subdirectory useful.
3. Feature services receive `context.Context` and explicit dependencies through constructors. Do not introduce globals or service locators.
4. External SDK usage stays in `internal/platform`. Add a narrow interface at the consuming feature boundary only when substitution is valuable, as `media` does for R2 storage.
5. Do not add `pkg/` until this repository intentionally publishes a reusable external Go library.
6. Do not add generic repositories, CQRS folders, or domain/application/infrastructure layers without a concrete need.
7. Database schema changes require an ordered forward and rollback migration; application startup never calls `AutoMigrate`.
8. HTTP request/response types stay close to their Gin handler. Domain/product state stays in feature models and services.
9. If background processing is introduced, add a separate `cmd/worker` composition root. Queue transport belongs under `internal/platform`; job rules stay with the owning feature. Do not run unbounded jobs inside HTTP handlers.

## Adding a feature

For a new capability such as `prompt` or `qrsource`, start with:

```text
internal/qrsource/
├── qrsource.go          # model and focused types
├── service.go           # business rules and persistence operations
├── handler.go           # HTTP validation and response mapping, if endpoints exist
└── service_test.go      # colocated unit tests
```

Then add a migration, wire the concrete service in `cmd/api/main.go`, and mount its handler in `internal/httpapi/router.go`. Use an adapter under `internal/platform` only when the feature talks to an external system.

## Naming system

- Directory and package names are lowercase singular nouns: `event`, `media`, `guest`.
- Interfaces describe capability: `Storage`, `Limiter`; implementations describe technology: `r2.R2`, `redis.Limiter`.
- Constructors use `New…`; request DTOs are private to the handler unless another transport genuinely shares them.
- Human-facing API paths use plural resources; internal product references use UUIDs, while public URLs use event slugs.
