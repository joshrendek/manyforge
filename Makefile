GO ?= go
PKG := ./...

.PHONY: build dev test sec-test lint vet generate migrate tidy

build:
	$(GO) build $(PKG)

dev:
	$(GO) run ./cmd/manyforge

test:
	$(GO) test $(PKG)

# Security-regression suite is the merge gate for Principles I/II/IV.
sec-test:
	@if [ -d internal/security_regression ]; then \
		$(GO) test ./internal/security_regression/... ; \
	else \
		echo "no internal/security_regression package yet"; \
	fi

vet:
	$(GO) vet $(PKG)

lint: vet
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run || echo "golangci-lint not installed; ran go vet only"

generate:
	sqlc generate

migrate:
	migrate -path migrations -database "$$MANYFORGE_DATABASE_URL" up

# Fast RLS isolation smoke check (connect as a superuser DSN; resets tenant data).
db-smoke:
	psql "$$MANYFORGE_DATABASE_URL" -v ON_ERROR_STOP=1 -tA -f db/tests/rls_smoke.sql

tidy:
	$(GO) mod tidy
