APP := candidcrowd-api
MIGRATE := migrate -path migrations -database "$(DATABASE_URL)"

.PHONY: dev run build test test-integration lint fmt vet docker-up docker-down migrate-up migrate-down migrate-create
dev: ; air
run: ; go run ./cmd/api
build: ; go build -o bin/$(APP) ./cmd/api
test: ; go test ./...
test-integration: ; docker compose -f docker-compose.test.yml up --abort-on-container-exit
lint: ; golangci-lint run
fmt: ; go fmt ./...
vet: ; go vet ./...
docker-up: ; docker compose up --build -d
docker-down: ; docker compose down
migrate-up: ; $(MIGRATE) up
migrate-down: ; $(MIGRATE) down 1
migrate-create: ; test -n "$(name)" && migrate create -ext sql -dir migrations -seq $(name)
