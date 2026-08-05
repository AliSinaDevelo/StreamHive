#!/usr/bin/env sh
set -eu

ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
BIN="$ROOT_DIR/bin/fs"
SOURCE_ADDR="${P2P_ADDR:-127.0.0.1:17070}"
SOURCE_HEALTH="${HEALTH_ADDR:-127.0.0.1:18080}"
TARGET_ADDR="${TARGET_P2P_ADDR:-127.0.0.1:17071}"
TARGET_HEALTH="${TARGET_HEALTH_ADDR:-127.0.0.1:18081}"
DATA_DIR="${STREAMHIVE_DATA_DIR:-$ROOT_DIR/.streamhive-continuation}"
SOURCE_DIR="$DATA_DIR/source"
TARGET_DIR="$DATA_DIR/target"
SOURCE_LOG="$(mktemp -t streamhive-continuation-source.XXXXXX)"
TARGET_LOG="$(mktemp -t streamhive-continuation-target.XXXXXX)"

source_pid=""
target_pid=""
cleanup() {
	if [ -n "$target_pid" ]; then
		kill "$target_pid" 2>/dev/null || true
		wait "$target_pid" 2>/dev/null || true
	fi
	if [ -n "$source_pid" ]; then
		kill "$source_pid" 2>/dev/null || true
		wait "$source_pid" 2>/dev/null || true
	fi
	rm -f "$SOURCE_LOG" "$TARGET_LOG"
}
trap cleanup EXIT INT TERM

wait_ready() {
	name="$1"
	url="$2"
	log_file="$3"
	i=0
	until curl -fsS "$url/readyz" >/dev/null 2>&1; do
		i=$((i + 1))
		if [ "$i" -gt 80 ]; then
			echo "$name did not become ready" >&2
			cat "$log_file" >&2 || true
			exit 1
		fi
		sleep 0.1
	done
}

wait_metric_value() {
	name="$1"
	url="$2"
	metric="$3"
	value="$4"
	log_file="$5"
	i=0
	until curl -fsS "$url/metrics" | grep "\"$metric\": $value" >/dev/null; do
		i=$((i + 1))
		if [ "$i" -gt 80 ]; then
			echo "$name did not report $metric=$value" >&2
			curl -fsS "$url/metrics" >&2 || true
			cat "$log_file" >&2 || true
			exit 1
		fi
		sleep 0.1
	done
}

wait_positive_metric() {
	name="$1"
	url="$2"
	metric="$3"
	log_file="$4"
	i=0
	until curl -fsS "$url/metrics" | grep "\"$metric\": [1-9]" >/dev/null; do
		i=$((i + 1))
		if [ "$i" -gt 80 ]; then
			echo "$name did not report a positive $metric" >&2
			curl -fsS "$url/metrics" >&2 || true
			cat "$log_file" >&2 || true
			exit 1
		fi
		sleep 0.1
	done
}

cleanup
rm -rf "$DATA_DIR"
mkdir -p "$SOURCE_DIR" "$TARGET_DIR" "$ROOT_DIR/bin"

go build -o "$BIN" "$ROOT_DIR"

"$BIN" \
	-listen "$SOURCE_ADDR" \
	-replicate \
	-store-dir "$SOURCE_DIR" \
	-max-repair-bytes 8 \
	-health "$SOURCE_HEALTH" \
	>"$SOURCE_LOG" 2>&1 &
source_pid=$!
wait_ready source "http://$SOURCE_HEALTH" "$SOURCE_LOG"

for key in alpha bravo charlie; do
	"$BIN" \
		-listen 127.0.0.1:0 \
		-dial "$SOURCE_ADDR" \
		-put-key "$key" \
		-put-data data \
		-exit-after-put \
		>/dev/null 2>&1
done

"$BIN" \
	-listen "$TARGET_ADDR" \
	-dial "$SOURCE_ADDR" \
	-replicate \
	-store-dir "$TARGET_DIR" \
	-max-repair-bytes 8 \
	-health "$TARGET_HEALTH" \
	>"$TARGET_LOG" 2>&1 &
target_pid=$!
wait_ready target "http://$TARGET_HEALTH" "$TARGET_LOG"

wait_metric_value target "http://$TARGET_HEALTH" replication_blobs_stored 3 "$TARGET_LOG"
wait_positive_metric source "http://$SOURCE_HEALTH" replication_repair_blobs_deferred "$SOURCE_LOG"
wait_positive_metric source "http://$SOURCE_HEALTH" replication_repair_continuations_scheduled "$SOURCE_LOG"
wait_positive_metric source "http://$SOURCE_HEALTH" replication_repair_continuations_completed "$SOURCE_LOG"

echo "bounded repair continuation demo passed: 3 blobs arrived with periodic sync disabled"
echo "source metrics:"
curl -fsS "http://$SOURCE_HEALTH/metrics"
echo "target metrics:"
curl -fsS "http://$TARGET_HEALTH/metrics"
