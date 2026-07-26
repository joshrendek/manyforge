#!/usr/bin/env bash
set -euo pipefail

test_tmp=$(mktemp -d)
container_id=""
app_container=""
postgres_container=""
test_network="manyforge-geoip-ci-${RANDOM}-$$"

cleanup() {
  if [[ -n "$app_container" ]]; then
    docker rm -f "$app_container" >/dev/null 2>&1 || true
  fi
  if [[ -n "$postgres_container" ]]; then
    docker rm -f "$postgres_container" >/dev/null 2>&1 || true
  fi
  if [[ -n "$container_id" ]]; then
    docker rm -f "$container_id" >/dev/null 2>&1 || true
  fi
  docker network rm "$test_network" >/dev/null 2>&1 || true
  docker image rm -f manyforge-geoip-credentialed-test manyforge-geoip-secretless-test \
    >/dev/null 2>&1 || true
  rm -rf "$test_tmp"
}
trap cleanup EXIT

account_sentinel="maxmind-account-sentinel-4f61a8"
license_sentinel="maxmind-license-sentinel-9c27d3"
printf '%s' "$account_sentinel" >"$test_tmp/account-id"
printf '%s' "$license_sentinel" >"$test_tmp/license-key"
chmod 0600 "$test_tmp/account-id" "$test_tmp/license-key"

DOCKER_BUILDKIT=1 docker build \
  --secret "id=maxmind_account_id,src=$test_tmp/account-id" \
  --secret "id=maxmind_license_key,src=$test_tmp/license-key" \
  --build-arg GEOIP_CACHE_KEY=credentialed-ci \
  --build-arg GEOIP_CREDENTIALS_PRESENT=true \
  --build-arg GEOIP_TEST_MODE=true \
  --tag manyforge-geoip-credentialed-test \
  . 2>&1 | tee "$test_tmp/credentialed-build.log"

container_id=$(docker create manyforge-geoip-credentialed-test /not-run)
docker export "$container_id" >"$test_tmp/credentialed-runtime.tar"
docker rm -f "$container_id" >/dev/null
container_id=""

tar -xOf "$test_tmp/credentialed-runtime.tar" geo/GeoLite2-Country.mmdb \
  >"$test_tmp/extracted.mmdb"
cmp scripts/ci/testdata/GeoLite2-Country-Test.mmdb "$test_tmp/extracted.mmdb"
docker history --no-trunc --format '{{.CreatedBy}}' manyforge-geoip-credentialed-test \
  >"$test_tmp/history"
for sentinel in "$account_sentinel" "$license_sentinel"; do
  if grep -aFq "$sentinel" "$test_tmp/credentialed-runtime.tar" ||
    grep -Fq "$sentinel" "$test_tmp/history" ||
    grep -Fq "$sentinel" "$test_tmp/credentialed-build.log"; then
    echo >&2 "MaxMind sentinel credential leaked into build output, runtime image, or history"
    exit 1
  fi
done

image_user=$(docker image inspect --format '{{.Config.User}}' manyforge-geoip-credentialed-test)
case "$image_user" in
  "" | 0 | 0:0 | root | root:root)
    echo >&2 "final app image must declare a non-root runtime user"
    exit 1
    ;;
esac

docker network create "$test_network" >/dev/null
postgres_container=$(docker run --detach \
  --network "$test_network" \
  --network-alias postgres \
  --env POSTGRES_USER=manyforge \
  --env POSTGRES_PASSWORD=devpassword \
  --env POSTGRES_DB=manyforge \
  postgres:16)
for _ in {1..100}; do
  if docker exec "$postgres_container" pg_isready --username manyforge --dbname manyforge \
    >/dev/null 2>&1; then
    break
  fi
  sleep 0.2
done
docker exec "$postgres_container" pg_isready --username manyforge --dbname manyforge >/dev/null

super_dsn='postgres://manyforge:devpassword@postgres:5432/manyforge?sslmode=disable'
app_dsn='postgres://manyforge_app:apppw@postgres:5432/manyforge?sslmode=disable'
docker run --rm \
  --network "$test_network" \
  --env "MANYFORGE_DATABASE_URL=$super_dsn" \
  manyforge-geoip-credentialed-test migrate
docker exec "$postgres_container" \
  psql --username manyforge --dbname manyforge --set ON_ERROR_STOP=1 \
  --command "ALTER ROLE manyforge_app LOGIN PASSWORD 'apppw'" >/dev/null

app_container=$(docker run --detach \
  --network "$test_network" \
  --env "MANYFORGE_DATABASE_URL=$app_dsn" \
  --env MANYFORGE_GEOIP_DB=/geo/GeoLite2-Country.mmdb \
  --env MANYFORGE_SANDBOX_MODE=off \
  manyforge-geoip-credentialed-test)
started=false
for _ in {1..100}; do
  docker logs "$app_container" >"$test_tmp/app.log" 2>&1 || true
  if grep -Fq "analytics geoip database loaded" "$test_tmp/app.log" &&
    grep -Fq "starting server" "$test_tmp/app.log"; then
    started=true
    break
  fi
  if [[ "$(docker inspect --format '{{.State.Running}}' "$app_container")" != "true" ]]; then
    cat "$test_tmp/app.log" >&2
    echo >&2 "final app image exited before startup completed"
    exit 1
  fi
  sleep 0.2
done
if [[ "$started" != "true" ]]; then
  cat "$test_tmp/app.log" >&2
  echo >&2 "final app image did not load GeoIP and start its server"
  exit 1
fi

DOCKER_BUILDKIT=1 docker build \
  --target runtime-geoip \
  --build-arg GEOIP_CACHE_KEY=secretless-ci \
  --build-arg GEOIP_CREDENTIALS_PRESENT=false \
  --tag manyforge-geoip-secretless-test \
  .

container_id=$(docker create manyforge-geoip-secretless-test /not-run)
docker export "$container_id" >"$test_tmp/secretless-runtime.tar"
docker rm -f "$container_id" >/dev/null
container_id=""
if tar -tf "$test_tmp/secretless-runtime.tar" | grep -q 'GeoLite2-Country\.mmdb$'; then
  echo >&2 "secretless GeoIP runtime unexpectedly contains a database"
  exit 1
fi
