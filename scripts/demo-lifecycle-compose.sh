#!/usr/bin/env sh
set -eu

ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/docker-compose.lifecycle.yml"
DATA_DIR="${STREAMHIVE_LIFECYCLE_DATA_DIR:-$ROOT_DIR/.streamhive-lifecycle}"
TOKEN="${STREAMHIVE_LIFECYCLE_TOKEN:-streamhive-lifecycle-demo-token}"
HEALTH_TOKEN="${STREAMHIVE_LIFECYCLE_HEALTH_TOKEN:-streamhive-lifecycle-health-demo-token}"
EXPECTED_KEY="2ec046e61b07981f2e389edfeafbfd5d08ffde38faf4512fc7dd725d507b94f2"
export STREAMHIVE_LIFECYCLE_DATA_DIR="$DATA_DIR"
export STREAMHIVE_LIFECYCLE_TOKEN="$TOKEN"
export STREAMHIVE_LIFECYCLE_HEALTH_TOKEN="$HEALTH_TOKEN"

curl_health() {
	curl -fsS -H "Authorization: Bearer $HEALTH_TOKEN" "$@"
}

compose() {
	docker compose -f "$COMPOSE_FILE" "$@"
}

cleanup() {
	compose down --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

wait_ready() {
	name="$1"
	url="$2"
	i=0
	until curl_health "$url/readyz" >/dev/null 2>&1; do
		i=$((i + 1))
		if [ "$i" -gt 120 ]; then
			echo "$name did not become ready" >&2
			compose logs "$name" >&2 || true
			exit 1
		fi
		sleep 0.25
	done
}

wait_status() {
	name="$1"
	url="$2"
	pattern="$3"
	i=0
	while :; do
		payload="$(curl_health "$url/lifecycle/status" 2>/dev/null || true)"
		if printf '%s\n' "$payload" | grep -F "$pattern" >/dev/null 2>&1; then
			return 0
		fi
		i=$((i + 1))
		if [ "$i" -gt 120 ]; then
			echo "$name did not report lifecycle status pattern: $pattern" >&2
			printf '%s\n' "$payload" >&2
			compose logs "$name" >&2 || true
			exit 1
		fi
		sleep 0.25
	done
}

floor_version() {
	url="$1"
	curl_health "$url/lifecycle/status" | awk -F: '
		/"journal_floor": \{/ { in_floor = 1; next }
		in_floor && /"epoch":/ { gsub(/[^0-9]/, "", $2); epoch = $2; next }
		in_floor && /"sequence":/ { gsub(/[^0-9]/, "", $2); print epoch "/" $2; exit }
	'
}

wait_floor_version() {
	name="$1"
	url="$2"
	want="$3"
	i=0
	while :; do
		got="$(floor_version "$url" 2>/dev/null || true)"
		if [ "$got" = "$want" ]; then
			return 0
		fi
		i=$((i + 1))
		if [ "$i" -gt 120 ]; then
			echo "$name did not reach lifecycle floor $want (got $got)" >&2
			compose logs "$name" >&2 || true
			exit 1
		fi
		sleep 0.25
	done
}

run_node1() {
	compose run --rm --no-deps node1 "$@"
}

node_keys() {
	node="$1"
	compose exec -T "$node" /usr/local/bin/streamhive -replicate -store-dir /data -list-keys
}

wait_raw_key() {
	node="$1"
	i=0
	until node_keys "$node" 2>/dev/null | grep -Fx "$EXPECTED_KEY" >/dev/null 2>&1; do
		i=$((i + 1))
		if [ "$i" -gt 120 ]; then
			echo "$node did not expose expected raw blob $EXPECTED_KEY" >&2
			compose logs "$node" >&2 || true
			exit 1
		fi
		sleep 0.25
	done
}

cleanup
rm -rf "$DATA_DIR"
mkdir -p "$DATA_DIR/node1/blobs" "$DATA_DIR/node1/lifecycle" \
	"$DATA_DIR/node2/blobs" "$DATA_DIR/node2/lifecycle" \
	"$DATA_DIR/node3/blobs" "$DATA_DIR/node3/lifecycle"
chmod -R 0777 "$DATA_DIR"

compose build node1
compose up -d node2 node3
wait_ready node2 http://127.0.0.1:18182
wait_ready node3 http://127.0.0.1:18183

run_node1 \
	-listen 127.0.0.1:0 \
	-replicate -store-dir /data \
	-lifecycle -lifecycle-dir /lifecycle \
	-peer-auth-token "$TOKEN" -peer-id node1 -peer-allow-ids node2,node3 \
	-peers node2:7070,node3:7070 \
	-lifecycle-members node2,node3 \
	-lifecycle-put-namespace demo -lifecycle-put-key live -lifecycle-put-data live-value \
	-lifecycle-exit-after-mutation

run_node1 \
	-listen 127.0.0.1:0 \
	-replicate -store-dir /data \
	-lifecycle -lifecycle-dir /lifecycle \
	-peer-auth-token "$TOKEN" -peer-id node1 -peer-allow-ids node2,node3 \
	-peers node2:7070,node3:7070 \
	-lifecycle-put-namespace demo -lifecycle-put-key retired -lifecycle-put-data retired-value \
	-lifecycle-exit-after-mutation

run_node1 \
	-listen 127.0.0.1:0 \
	-replicate -store-dir /data \
	-lifecycle -lifecycle-dir /lifecycle \
	-peer-auth-token "$TOKEN" -peer-id node1 -peer-allow-ids node2,node3 \
	-peers node2:7070,node3:7070 \
	-lifecycle-delete-namespace demo -lifecycle-delete-key retired \
	-lifecycle-exit-after-mutation

compose up -d node1
wait_ready node1 http://127.0.0.1:18181
wait_status node1 http://127.0.0.1:18181 '"membership_acknowledged": 2'
wait_status node2 http://127.0.0.1:18182 '"logical_records": 2'
wait_status node2 http://127.0.0.1:18182 '"tombstones": 1'
wait_status node3 http://127.0.0.1:18183 '"logical_records": 2'
wait_status node3 http://127.0.0.1:18183 '"tombstones": 1'
wait_raw_key node3

compose stop node3 >/dev/null
compose rm -f node3 >/dev/null
compose stop node1 >/dev/null
compose rm -f node1 >/dev/null

compact_output="$(run_node1 \
	-listen 127.0.0.1:0 \
	-replicate -store-dir /data \
	-lifecycle -lifecycle-dir /lifecycle \
	-peer-auth-token "$TOKEN" -peer-id node1 \
	-lifecycle-compact)"
case "$compact_output" in
	*"lifecycle compacted watermark="*"members=2"*) ;;
	*)
		echo "source compaction did not report two configured members" >&2
		printf '%s\n' "$compact_output" >&2
		exit 1
		;;
esac

compose up -d node1
wait_ready node1 http://127.0.0.1:18181
wait_status node1 http://127.0.0.1:18181 '"journal_entries": 0'
wait_status node1 http://127.0.0.1:18181 '"logical_records": 2'
wait_status node1 http://127.0.0.1:18181 '"tombstones": 1'
source_floor="$(floor_version http://127.0.0.1:18181)"
case "$source_floor" in
	*/[1-9]*) ;;
	*)
		echo "source did not restore a non-zero compacted journal floor: $source_floor" >&2
		exit 1
		;;
esac

if [ ! -f "$DATA_DIR/node3/blobs/$EXPECTED_KEY" ]; then
	echo "target raw blob disappeared before stale lifecycle restart" >&2
	exit 1
fi
rm -rf "$DATA_DIR/node3/lifecycle"
mkdir -p "$DATA_DIR/node3/lifecycle"
chmod 0777 "$DATA_DIR/node3/lifecycle"

compose up -d node3
wait_ready node3 http://127.0.0.1:18183
wait_status node3 http://127.0.0.1:18183 '"logical_records": 2'
wait_status node3 http://127.0.0.1:18183 '"tombstones": 1'
wait_status node3 http://127.0.0.1:18183 '"journal_entries": 0'
wait_floor_version node3 http://127.0.0.1:18183 "$source_floor"
wait_raw_key node3

target_hash="$(compose exec -T node3 sha256sum "/data/$EXPECTED_KEY" | awk '{print $1}')"
if [ "$target_hash" != "$EXPECTED_KEY" ]; then
	echo "target raw blob hash changed after snapshot repair: $target_hash" >&2
	exit 1
fi

echo "3-node lifecycle compose demo passed: stale target restored compacted snapshot after source restart"
echo "checkpoint floor: $source_floor"
echo "retained raw key: $EXPECTED_KEY"
curl_health http://127.0.0.1:18183/lifecycle/status
