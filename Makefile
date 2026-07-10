# Optional: absent in CI (drift jobs don't need it), required for dev targets.
-include .env
export

COMPOSE := docker compose -f deploy/docker-compose.yml --env-file .env
MIGRATIONS := $(CURDIR)/db/migrations
# Runs on the compose network so `postgres` resolves on macOS and Linux alike.
MIGRATE := docker run --rm -v $(MIGRATIONS):/migrations --network sdano_default \
  migrate/migrate:v4.19.1 -path=/migrations \
  -database "postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@postgres:5432/$(POSTGRES_DB)?sslmode=disable"

.PHONY: dev-up dev-down migrate-up migrate-down migrate-drop generate-sqlc openapi generate-client generate lint test drift report-preview

dev-up:
	$(COMPOSE) --profile dev up -d --wait postgres minio headless-shell
	$(COMPOSE) --profile dev up -d minio-setup
	$(COMPOSE) --profile dev up -d api

dev-down:
	$(COMPOSE) --profile dev down

migrate-up:
	$(MIGRATE) up

migrate-down:
	$(MIGRATE) down 1

migrate-drop:
	$(MIGRATE) drop -f

generate-sqlc:
	sqlc generate

openapi:
	cd apps/api && go run ./cmd/api openapi > ../../packages/api-client/openapi.json

generate-client: openapi
	cd packages/api-client && npm run generate

generate: generate-sqlc generate-client

lint:
	cd apps/api && golangci-lint run

test:
	cd apps/api && go test ./...

drift: generate
	git diff --exit-code

report-preview:
	cd apps/api && go run ./cmd/report-preview
