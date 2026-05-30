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

tidy:
	$(GO) mod tidy
