#!/usr/bin/env bash
set -euo pipefail

test_tmp=$(mktemp -d)
mock_pid=""
container_id=""

cleanup() {
  if [[ -n "$container_id" ]]; then
    docker rm -f "$container_id" >/dev/null 2>&1 || true
  fi
  if [[ -n "$mock_pid" ]]; then
    kill "$mock_pid" >/dev/null 2>&1 || true
    wait "$mock_pid" >/dev/null 2>&1 || true
  fi
  docker image rm -f manyforge-geoip-credentialed-test manyforge-geoip-secretless-test \
    >/dev/null 2>&1 || true
  rm -rf "$test_tmp"
}
trap cleanup EXIT

account_sentinel="maxmind-account-sentinel-4f61a8"
license_sentinel="maxmind-license-sentinel-9c27d3"
archive_root="$test_tmp/archive/GeoLite2-Country_test"
mkdir -p "$archive_root"
printf 'CI GeoLite2 fixture\n' >"$archive_root/GeoLite2-Country.mmdb"
tar -czf "$test_tmp/geolite.tar.gz" -C "$test_tmp/archive" .
printf '%s' "$account_sentinel" >"$test_tmp/account-id"
printf '%s' "$license_sentinel" >"$test_tmp/license-key"
chmod 0600 "$test_tmp/account-id" "$test_tmp/license-key"

export GEOIP_TEST_ARCHIVE="$test_tmp/geolite.tar.gz"
export GEOIP_TEST_ACCOUNT="$account_sentinel"
export GEOIP_TEST_LICENSE="$license_sentinel"
python3 - <<'PY' &
import base64
import http.server
import os
from pathlib import Path

archive = Path(os.environ["GEOIP_TEST_ARCHIVE"]).read_bytes()
credentials = f'{os.environ["GEOIP_TEST_ACCOUNT"]}:{os.environ["GEOIP_TEST_LICENSE"]}'
expected_auth = "Basic " + base64.b64encode(credentials.encode()).decode()


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != "/geolite.tar.gz" or self.headers.get("Authorization") != expected_auth:
            self.send_response(401)
            self.end_headers()
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/gzip")
        self.send_header("Content-Length", str(len(archive)))
        self.end_headers()
        self.wfile.write(archive)

    def log_message(self, _format, *_args):
        pass


http.server.ThreadingHTTPServer(("127.0.0.1", 18080), Handler).serve_forever()
PY
mock_pid=$!

for _ in {1..50}; do
  if curl --silent --output /dev/null http://127.0.0.1:18080/geolite.tar.gz; then
    break
  fi
  sleep 0.1
done
kill -0 "$mock_pid"

DOCKER_BUILDKIT=1 docker build \
  --network host \
  --target runtime-geoip \
  --secret "id=maxmind_account_id,src=$test_tmp/account-id" \
  --secret "id=maxmind_license_key,src=$test_tmp/license-key" \
  --build-arg GEOIP_CACHE_KEY=credentialed-ci \
  --build-arg GEOIP_CREDENTIALS_PRESENT=true \
  --build-arg GEOIP_DOWNLOAD_URL=http://127.0.0.1:18080/geolite.tar.gz \
  --tag manyforge-geoip-credentialed-test \
  . 2>&1 | tee "$test_tmp/credentialed-build.log"

container_id=$(docker create manyforge-geoip-credentialed-test /not-run)
docker export "$container_id" >"$test_tmp/credentialed-runtime.tar"
docker rm -f "$container_id" >/dev/null
container_id=""

tar -xOf "$test_tmp/credentialed-runtime.tar" geo/GeoLite2-Country.mmdb \
  >"$test_tmp/extracted.mmdb"
cmp "$archive_root/GeoLite2-Country.mmdb" "$test_tmp/extracted.mmdb"
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
