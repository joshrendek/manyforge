#!/usr/bin/env bash
set -euo pipefail

test_tmp=$(mktemp -d)
container_id=""

cleanup() {
  if [[ -n "$container_id" ]]; then
    docker rm -f "$container_id" >/dev/null 2>&1 || true
  fi
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
  --target runtime-geoip \
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
printf 'CI GeoLite2 fixture\n' >"$test_tmp/expected.mmdb"
cmp "$test_tmp/expected.mmdb" "$test_tmp/extracted.mmdb"
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
