#!/usr/bin/env sh
set -eu

ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
DATA_DIR="${STREAMHIVE_DATA_DIR:-$ROOT_DIR/.streamhive-repair}"
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

wait_metric() {
	name="$1"
	url="$2"
	metric="$3"
	i=0
	until curl_health "$url/metrics" | grep "\"$metric\": [1-9]" >/dev/null; do
		i=$((i + 1))
		if [ "$i" -gt 80 ]; then
			echo "$name did not report a positive $metric counter" >&2
			curl_health "$url/metrics" >&2 || true
			$COMPOSE -f "$ROOT_DIR/docker-compose.yml" logs "$name" >&2 || true
			exit 1
		fi
		sleep 0.25
	done
}

node_keys() {
	node="$1"
	$COMPOSE -f "$ROOT_DIR/docker-compose.yml" --profile tools run --rm --no-deps -v "$DATA_DIR/$node:/data" seed -store-dir /data -list-keys
}

wait_key_present() {
	node="$1"
	i=0
	until node_keys "$node" | grep "$EXPECTED_KEY" >/dev/null; do
		i=$((i + 1))
		if [ "$i" -gt 80 ]; then
			echo "$node store did not contain expected key $EXPECTED_KEY" >&2
			$COMPOSE -f "$ROOT_DIR/docker-compose.yml" logs "$node" >&2 || true
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
$COMPOSE -f "$ROOT_DIR/docker-compose.yml" up -d node1 node2 node3
wait_ready node1 http://127.0.0.1:18081
wait_ready node2 http://127.0.0.1:18082
wait_ready node3 http://127.0.0.1:18083

$COMPOSE -f "$ROOT_DIR/docker-compose.yml" --profile tools run --rm seed
wait_key_present node3

$COMPOSE -f "$ROOT_DIR/docker-compose.yml" exec -T node3 sh -c "printf '%s' 'tampered' > '/data/$EXPECTED_KEY'"
if [ ! -f "$DATA_DIR/node3/$EXPECTED_KEY" ]; then
	echo "node3 did not retain expected key after local corruption" >&2
	exit 1
fi

wait_key_present node3
wait_metric node3 http://127.0.0.1:18083 replication_corrupt_blobs_detected
wait_metric node1 http://127.0.0.1:18081 replication_repair_blobs_sent

repaired_hash="$($COMPOSE -f "$ROOT_DIR/docker-compose.yml" exec -T node3 sha256sum "/data/$EXPECTED_KEY" | awk '{print $1}')"
if [ "$repaired_hash" != "$EXPECTED_KEY" ]; then
	echo "node3 content hash did not recover after corruption: $repaired_hash" >&2
	exit 1
fi

echo "3-node repair demo passed: node3 detected and repaired corrupted blob"
echo "repaired key: $EXPECTED_KEY"
curl_health http://127.0.0.1:18083/metrics
echo
