# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
# Dockerfile — manyforge app image.
#
# Three stages: build the Angular SPA, embed it into the Go binary behind the
# `ui_embed` build tag (internal/webui/embed.go expects the built SPA at
# internal/webui/dist, matching //go:embed all:dist), then ship a distroless
# non-root runtime with the binary + migrations.
#
# `manyforge migrate` resolves its migrations dir as a relative path ("migrations",
# see cmd/manyforge/main.go -> db.Migrate(cfg.DatabaseURL, "migrations") and
# internal/platform/db/migrate.go's migrate.New("file://"+migrationsDir, ...)), so
# WORKDIR must be "/" with the migrations tree copied to "/migrations".

# 1. Angular SPA
FROM node:22-bookworm-slim@sha256:de170b39439eb0e5a3444ad7d60b015614a381e10fecae2e30ec9e735122f38d AS web
WORKDIR /web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
# @angular/build:application's default outputPath is dist/<project>/browser
# (no outputPath override in web/angular.json) -> web/dist/manyforge-web/browser/.
RUN npm run build

# 2. Go build with embedded SPA
FROM golang:1.26.10@sha256:672c944cfd07599b5f1bb179869270cdc94c6254e41db4d431259693cb286b82 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Place the built SPA where //go:embed all:dist (internal/webui/embed.go) expects
# it: internal/webui/dist/. Overwrites the committed placeholder index.html.
RUN rm -rf internal/webui/dist && mkdir -p internal/webui/dist
COPY --from=web /web/dist/manyforge-web/browser/ internal/webui/dist/
RUN CGO_ENABLED=0 go build -tags ui_embed -trimpath -ldflags="-s -w" -o /manyforge ./cmd/manyforge
# Work Board CLI (spec 018): cross-compiled for every platform internal/clidist
# serves at /cli/mf/{os}/{arch}, so repos and sandboxes can install `mf` from
# their own instance (curl <instance>/cli/install.sh | sh).
ARG MF_VERSION=image
RUN mkdir -p /cli && for p in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64; do \
      CGO_ENABLED=0 GOOS=${p%/*} GOARCH=${p#*/} go build -trimpath \
        -ldflags="-s -w -X main.version=${MF_VERSION}" -o /cli/mf-${p%/*}-${p#*/} ./cmd/mf || exit 1; \
    done

# 3. Runtime
FROM gcr.io/distroless/static:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
COPY --from=build /manyforge /manyforge
COPY --from=build /src/migrations /migrations
COPY --from=build /cli /cli
USER nonroot:nonroot
WORKDIR /
EXPOSE 8080
ENTRYPOINT ["/manyforge"]
