#!/usr/bin/env sh
set -eu

ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
DATA_DIR="${STREAMHIVE_DATA_DIR:-$ROOT_DIR/.streamhive-auth}"
COMPOSE="docker compose"
EXPECTED_KEY="cd13ac0817f0f8ba2f29fba23617ef0191a6193ed0311298163834199398ee05"
TOKEN="${STREAMHIVE_PEER_TOKEN:-streamhive-compose-demo-token}"
WRONG_TOKEN="${STREAMHIVE_WRONG_PEER_TOKEN:-streamhive-invalid-token}"
if [ "$TOKEN" = "$WRONG_TOKEN" ]; then
	WRONG_TOKEN="${TOKEN}!"
fi
export STREAMHIVE_DATA_DIR="$DATA_DIR"
export STREAMHIVE_PEER_TOKEN="$TOKEN"

cleanup() {
	$COMPOSE -f "$ROOT_DIR/docker-compose.yml" down --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

wait_ready() {
	name="$1"
	url="$2"
	i=0
	until curl -fsS "$url/readyz" >/dev/null 2>&1; do
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
	until curl -fsS "$url/metrics" | grep "\"$metric\": [1-9]" >/dev/null; do
		i=$((i + 1))
		if [ "$i" -gt 80 ]; then
			echo "$name did not report a positive $metric counter" >&2
			curl -fsS "$url/metrics" >&2 || true
			$COMPOSE -f "$ROOT_DIR/docker-compose.yml" logs "$name" >&2 || true
			exit 1
		fi
		sleep 0.25
	done
}

wait_stored() {
	name="$1"
	url="$2"
	wait_metric "$name" "$url" replication_blobs_stored
}

node_keys() {
	node="$1"
	$COMPOSE -f "$ROOT_DIR/docker-compose.yml" --profile tools run --rm --no-deps \
		-v "$DATA_DIR/$node:/data" seed -store-dir /data -list-keys
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
wait_metric node1 http://127.0.0.1:18081 peer_auth_success

$COMPOSE -f "$ROOT_DIR/docker-compose.yml" --profile tools run --rm seed
wait_stored node3 http://127.0.0.1:18083

if $COMPOSE -f "$ROOT_DIR/docker-compose.yml" --profile tools run --rm --no-deps seed \
	-listen 127.0.0.1:0 \
	-dial node1:7070 \
	-peer-auth-token "$WRONG_TOKEN" \
	-put-key unauthorized-key \
	-put-data "this blob must be rejected" \
	-exit-after-put; then
	echo "wrong-token sender unexpectedly completed" >&2
	exit 1
fi

wait_metric node1 http://127.0.0.1:18081 peer_auth_failures
if node_keys node1 | grep '^unauthorized-key$' >/dev/null; then
	echo "wrong-token sender stored unauthorized-key" >&2
	exit 1
fi

echo "authenticated 3-node compose demo passed: matching token replicated; wrong token rejected"
echo "replicated key: $EXPECTED_KEY"
curl -fsS http://127.0.0.1:18081/metrics
echo
