APP := candidcrowd-api
MIGRATE := migrate -path migrations -database "$(DATABASE_URL)"

.PHONY: dev run run-worker build test test-integration test-integration-db lint fmt vet docker-up docker-down migrate-up migrate-down migrate-create
dev: ; air
run: ; go run ./cmd/api
run-worker: ; RUN_WORKER=true go run ./cmd/api
build: ; go build -o bin/$(APP) ./cmd/api
test: ; go test ./...
test-integration: ; docker compose -f docker-compose.test.yml up --abort-on-container-exit
# -p 1: the packages share one database, and the plan backfill test acts on
# every event in it, so packages must not run concurrently.
test-integration-db: ; go test -tags integration -count=1 -p 1 ./internal/platform/database/ ./internal/entitlement/ ./internal/httpapi/ ./internal/retention/ ./internal/billing/
lint: ; golangci-lint run
fmt: ; go fmt ./...
vet: ; go vet ./...
docker-up: ; docker compose up --build -d
docker-down: ; docker compose down
migrate-up: ; @$(MIGRATE) up
migrate-down: ; @$(MIGRATE) down 1
migrate-create: ; test -n "$(name)" && migrate create -ext sql -dir migrations -seq $(name)
