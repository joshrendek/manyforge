#!/bin/sh
set -eu

netrc_file=""
output_file=""
download_url=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --netrc-file)
      netrc_file=$2
      shift 2
      ;;
    -o)
      output_file=$2
      shift 2
      ;;
    --retry)
      shift 2
      ;;
    --fail | --show-error | --silent | --location)
      shift
      ;;
    *)
      download_url=$1
      shift
      ;;
  esac
done

expected_url='https://download.maxmind.com/geoip/databases/GeoLite2-Country/download?suffix=tar.gz'
expected_netrc='machine download.maxmind.com login maxmind-account-sentinel-4f61a8 password maxmind-license-sentinel-9c27d3'

[ "$download_url" = "$expected_url" ]
[ -n "$netrc_file" ]
[ -n "$output_file" ]
[ "$(cat "$netrc_file")" = "$expected_netrc" ]

fixture_tmp=$(mktemp -d)
trap 'rm -rf "$fixture_tmp"' EXIT HUP INT TERM
mkdir -p "$fixture_tmp/GeoLite2-Country_test"
cp /tmp/geoip-ci/testdata/GeoLite2-Country-Test.mmdb \
  "$fixture_tmp/GeoLite2-Country_test/GeoLite2-Country.mmdb"
tar -czf "$output_file" -C "$fixture_tmp" .
