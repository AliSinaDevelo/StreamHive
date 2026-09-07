#!/usr/bin/env sh
set -eu

ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
DATA_DIR="${STREAMHIVE_DATA_DIR:-$ROOT_DIR/.streamhive-compose}"
COMPOSE="docker compose"
HEALTH_TOKEN="${STREAMHIVE_HEALTH_TOKEN:-streamhive-health-demo-token}"
export STREAMHIVE_DATA_DIR="$DATA_DIR"

curl_health() {
	curl -fsS -H "Authorization: Bearer $HEALTH_TOKEN" "$@"
}

print_node() {
	name="$1"
	url="$2"

	echo "== $name =="
	echo "-- peers --"
	curl_health "$url/peers"
	echo
	echo "-- metrics --"
	curl_health "$url/metrics"
	echo
	echo "-- durable keys --"
	keys="$($COMPOSE -f "$ROOT_DIR/docker-compose.yml" --profile tools run --rm --no-deps -v "$DATA_DIR/$name:/data" seed -store-dir /data -list-keys)"
	if [ -n "$keys" ]; then
		printf '%s\n' "$keys" | sed 's/^/  /'
	else
		echo "  (none)"
	fi
	echo
}

print_node node1 http://127.0.0.1:18081
print_node node2 http://127.0.0.1:18082
print_node node3 http://127.0.0.1:18083
