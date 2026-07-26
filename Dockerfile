# Dockerfile — manyforge app image.
#
# Build the Angular SPA, embed it into the Go binary behind the `ui_embed` build tag
# (internal/webui/embed.go expects the built SPA at internal/webui/dist, matching
# //go:embed all:dist), then ship a distroless non-root runtime with the binary,
# migrations, and optional GeoLite2 database.
#
# `manyforge migrate` resolves its migrations dir as a relative path ("migrations",
# see cmd/manyforge/main.go -> db.Migrate(cfg.DatabaseURL, "migrations") and
# internal/platform/db/migrate.go's migrate.New("file://"+migrationsDir, ...)), so
# WORKDIR must be "/" with the migrations tree copied to "/migrations".

# GeoLite2 Country is downloaded only when both BuildKit secrets are present. The account ID and
# license key must never be build args: args are recorded in image metadata/history. The public
# date and credential-presence args invalidate BuildKit's secret-insensitive cache.
# GEOIP_CREDENTIALS_PRESENT intentionally reveals only the configured/not-configured state in image
# history; it never carries either credential value.
FROM debian:bookworm-slim AS geoip
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /geo
ARG GEOIP_CACHE_KEY=manual
ARG GEOIP_CREDENTIALS_PRESENT=false
ARG GEOIP_TEST_MODE=false
RUN --mount=type=secret,id=maxmind_account_id \
    --mount=type=secret,id=maxmind_license_key \
    --mount=type=bind,source=scripts/ci,target=/tmp/geoip-ci,readonly \
    set -eu; \
    printf '%s:%s:%s' "$GEOIP_CACHE_KEY" "$GEOIP_CREDENTIALS_PRESENT" "$GEOIP_TEST_MODE" >/dev/null; \
    account_file=/run/secrets/maxmind_account_id; \
    license_file=/run/secrets/maxmind_license_key; \
    if [ ! -s "$account_file" ] || [ ! -s "$license_file" ]; then \
      echo >&2 "GeoLite2 Country not embedded: MaxMind build secrets are not configured"; \
      exit 0; \
    fi; \
    account_id="$(tr -d '\r\n' < "$account_file")"; \
    license_key="$(tr -d '\r\n' < "$license_file")"; \
    test -n "$account_id" && test -n "$license_key"; \
    umask 077; \
    printf 'machine download.maxmind.com login %s password %s\n' \
      "$account_id" "$license_key" > /tmp/maxmind.netrc; \
    trap 'rm -f /tmp/maxmind.netrc' EXIT HUP INT TERM; \
    mkdir -p /tmp/geolite; \
    download_client=curl; \
    if [ "$GEOIP_TEST_MODE" = true ]; then download_client=/tmp/geoip-ci/mock-maxmind-download.sh; fi; \
    "$download_client" --fail --show-error --silent --location --retry 3 \
      --netrc-file /tmp/maxmind.netrc \
      'https://download.maxmind.com/geoip/databases/GeoLite2-Country/download?suffix=tar.gz' \
      -o /tmp/geolite.tar.gz; \
    tar -xzf /tmp/geolite.tar.gz -C /tmp/geolite; \
    find /tmp/geolite -type f -name GeoLite2-Country.mmdb \
      -exec cp '{}' /geo/GeoLite2-Country.mmdb ';'; \
    test -s /geo/GeoLite2-Country.mmdb; \
    chmod 0444 /geo/GeoLite2-Country.mmdb

# The final app stage inherits this exact runtime base. CI can therefore validate both credentialed
# and secretless GeoIP filesystem content without rebuilding the Angular and Go applications.
FROM gcr.io/distroless/static:nonroot AS runtime-geoip
COPY --from=geoip /geo/ /geo/

# 1. Angular SPA
FROM node:20-bookworm-slim AS web
WORKDIR /web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
# @angular/build:application's default outputPath is dist/<project>/browser
# (no outputPath override in web/angular.json) -> web/dist/manyforge-web/browser/.
RUN npm run build

# 2. Go build with embedded SPA
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Place the built SPA where //go:embed all:dist (internal/webui/embed.go) expects
# it: internal/webui/dist/. Overwrites the committed placeholder index.html.
RUN rm -rf internal/webui/dist && mkdir -p internal/webui/dist
COPY --from=web /web/dist/manyforge-web/browser/ internal/webui/dist/
RUN CGO_ENABLED=0 go build -tags ui_embed -trimpath -ldflags="-s -w" -o /manyforge ./cmd/manyforge

# 3. Runtime
FROM runtime-geoip
COPY --from=build /manyforge /manyforge
COPY --from=build /src/migrations /migrations
USER nonroot:nonroot
WORKDIR /
EXPOSE 8080
ENTRYPOINT ["/manyforge"]
