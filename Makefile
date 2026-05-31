GO ?= go
PKG := ./...

.PHONY: build dev test sec-test int-test contract-test lint vet generate migrate db-smoke tidy

build:
	$(GO) build $(PKG)

dev:
	$(GO) run ./cmd/manyforge

test:
	$(GO) test $(PKG)

# Security-regression suite is the merge gate for Principles I/II/IV.
# DB-backed tests build under the `integration` tag and spin ephemeral Postgres
# via testcontainers (Docker required).
sec-test:
	$(GO) test -tags integration -timeout 600s ./internal/security_regression/...

# All integration tests (testcontainers; Docker required). Superset of sec-test.
int-test:
	$(GO) test -tags integration -timeout 600s ./...

# Shared-layer interface contracts (InboundSource, Blob, Notifier, event bus)
# plus the support OpenAPI-drift checks. Tag-gated so it can grow independently
# of the fast unit suite; no Docker required.
contract-test:
	$(GO) test -tags contract -timeout 120s ./...

vet:
	$(GO) vet $(PKG)

lint: vet
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run; else echo "golangci-lint not installed; ran go vet only"; fi

generate:
	sqlc generate

migrate:
	migrate -path migrations -database "$$MANYFORGE_DATABASE_URL" up

# Fast RLS isolation smoke check (connect as a superuser DSN; resets tenant data).
db-smoke:
	psql "$$MANYFORGE_DATABASE_URL" -v ON_ERROR_STOP=1 -tA -f db/tests/rls_smoke.sql

tidy:
	$(GO) mod tidy
