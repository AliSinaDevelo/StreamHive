#!/usr/bin/env sh
set -eu

ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
DATA_DIR="${STREAMHIVE_DATA_DIR:-$ROOT_DIR/.streamhive-compose}"
COMPOSE="docker compose"
EXPECTED_KEY="cd13ac0817f0f8ba2f29fba23617ef0191a6193ed0311298163834199398ee05"
HEALTH_TOKEN="${STREAMHIVE_HEALTH_TOKEN:-streamhive-health-demo-token}"
export STREAMHIVE_DATA_DIR="$DATA_DIR"
export STREAMHIVE_HEALTH_TOKEN="$HEALTH_TOKEN"

curl_health() {
	curl -fsS -H "Authorization: Bearer $HEALTH_TOKEN" "$@"
}

cleanup() {
	$COMPOSE -f "$ROOT_DIR/docker-compose.yml" down --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

wait_ready() {
	name="$1"
	url="$2"
	i=0
	until curl_health "$url/readyz" >/dev/null 2>&1; do
		i=$((i + 1))
		if [ "$i" -gt 80 ]; then
			echo "$name did not become ready" >&2
			$COMPOSE -f "$ROOT_DIR/docker-compose.yml" logs "$name" >&2 || true
			exit 1
		fi
		sleep 0.25
	done
}

wait_stored() {
	name="$1"
	url="$2"
	i=0
	until curl_health "$url/metrics" | grep '"replication_blobs_stored": [1-9]' >/dev/null; do
		i=$((i + 1))
		if [ "$i" -gt 80 ]; then
			echo "$name did not store replicated blob" >&2
			$COMPOSE -f "$ROOT_DIR/docker-compose.yml" logs "$name" >&2 || true
			exit 1
		fi
		sleep 0.25
	done
}

cleanup
rm -rf "$DATA_DIR"
mkdir -p "$DATA_DIR/node1" "$DATA_DIR/node2" "$DATA_DIR/node3"
chmod -R 0777 "$DATA_DIR"

$COMPOSE -f "$ROOT_DIR/docker-compose.yml" build
$COMPOSE -f "$ROOT_DIR/docker-compose.yml" up -d node1
wait_ready node1 http://127.0.0.1:18081

$COMPOSE -f "$ROOT_DIR/docker-compose.yml" --profile tools run --rm seed
$COMPOSE -f "$ROOT_DIR/docker-compose.yml" up -d node2 node3
wait_ready node2 http://127.0.0.1:18082
wait_ready node3 http://127.0.0.1:18083
wait_stored node3 http://127.0.0.1:18083

$COMPOSE -f "$ROOT_DIR/docker-compose.yml" stop node3
$COMPOSE -f "$ROOT_DIR/docker-compose.yml" rm -f node3
rm -rf "$DATA_DIR/node3"
mkdir -p "$DATA_DIR/node3"
chmod 0777 "$DATA_DIR/node3"
$COMPOSE -f "$ROOT_DIR/docker-compose.yml" up -d node3
wait_ready node3 http://127.0.0.1:18083
wait_stored node3 http://127.0.0.1:18083

keys="$($COMPOSE -f "$ROOT_DIR/docker-compose.yml" --profile tools run --rm --no-deps -v "$DATA_DIR/node3:/data" seed -store-dir /data -list-keys)"
case "$keys" in
	*"$EXPECTED_KEY"*) ;;
	*)
		echo "node3 store did not contain expected content key $EXPECTED_KEY" >&2
		echo "$keys" >&2
		exit 1
		;;
esac

echo "3-node compose demo passed: node3 rehydrated the blob after restart"
echo "rehydrated key: $EXPECTED_KEY"
curl_health http://127.0.0.1:18083/metrics
echo
"$ROOT_DIR/scripts/demo-status.sh"
