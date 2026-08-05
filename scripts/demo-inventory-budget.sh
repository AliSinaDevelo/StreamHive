#!/usr/bin/env sh
set -eu

ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
BIN="$ROOT_DIR/bin/fs"
SOURCE_ADDR="${P2P_ADDR:-127.0.0.1:17100}"
SOURCE_HEALTH="${HEALTH_ADDR:-127.0.0.1:18100}"
TARGET_ADDR="${TARGET_P2P_ADDR:-127.0.0.1:17101}"
TARGET_HEALTH="${TARGET_HEALTH_ADDR:-127.0.0.1:18101}"
DATA_DIR="${STREAMHIVE_DATA_DIR:-$ROOT_DIR/.streamhive-inventory-budget}"
SOURCE_DIR="$DATA_DIR/source"
TARGET_DIR="$DATA_DIR/target"
SOURCE_LOG="$(mktemp -t streamhive-inventory-budget-source.XXXXXX)"
TARGET_LOG="$(mktemp -t streamhive-inventory-budget-target.XXXXXX)"
EXPECTED_KEYS=""

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
		if [ "$i" -gt 100 ]; then
			echo "$name did not become ready" >&2
			cat "$log_file" >&2 || true
			exit 1
		fi
		sleep 0.1
	done
}

metric_value() {
	url="$1"
	metric="$2"
	curl -fsS "$url/metrics" | awk -v metric="$metric" '$0 ~ "\\\"" metric "\\\"" { gsub(/[^0-9-]/, "", $2); print $2; exit }'
}

wait_metric_at_least() {
	name="$1"
	url="$2"
	metric="$3"
	minimum="$4"
	log_file="$5"
	i=0
	while :; do
		value="$(metric_value "$url" "$metric" 2>/dev/null || true)"
		if [ -n "$value" ] && [ "$value" -ge "$minimum" ]; then
			return 0
		fi
		i=$((i + 1))
		if [ "$i" -gt 100 ]; then
			echo "$name did not report $metric >= $minimum (last=$value)" >&2
			curl -fsS "$url/metrics" >&2 || true
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
	expected="$4"
	log_file="$5"
	i=0
	while :; do
		value="$(metric_value "$url" "$metric" 2>/dev/null || true)"
		if [ "$value" = "$expected" ]; then
			return 0
		fi
		i=$((i + 1))
		if [ "$i" -gt 100 ]; then
			echo "$name did not report $metric=$expected (last=$value)" >&2
			curl -fsS "$url/metrics" >&2 || true
			cat "$log_file" >&2 || true
			exit 1
		fi
		sleep 0.1
	done
}

wait_keys() {
	name="$1"
	dir="$2"
	log_file="$3"
	i=0
	while :; do
		actual="$($BIN -store-dir "$dir" -list-keys 2>/dev/null || true)"
		all_present=true
		for key in $EXPECTED_KEYS; do
			if ! printf '%s\n' "$actual" | grep -Fx "$key" >/dev/null; then
				all_present=false
				break
			fi
		done
		if [ "$all_present" = true ]; then
			return 0
		fi
		i=$((i + 1))
		if [ "$i" -gt 100 ]; then
			echo "$name store did not converge" >&2
			echo "expected keys: $EXPECTED_KEYS" >&2
			echo "actual keys: $actual" >&2
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
	-sync-interval 0s \
	-max-inventory-bytes 128 \
	-max-inventory-keys 1 \
	-health "$SOURCE_HEALTH" \
	>"$SOURCE_LOG" 2>&1 &
source_pid=$!
wait_ready source "http://$SOURCE_HEALTH" "$SOURCE_LOG"

for index in 00 01 02 03 04 05 06 07; do
	data="streamhive-budgeted-inventory-blob-$index"
	if command -v sha256sum >/dev/null 2>&1; then
		key="$(printf '%s' "$data" | sha256sum | awk '{print $1}')"
	else
		key="$(printf '%s' "$data" | shasum -a 256 | awk '{print $1}')"
	fi
	EXPECTED_KEYS="$EXPECTED_KEYS $key"
	"$BIN" \
		-listen 127.0.0.1:0 \
		-dial "$SOURCE_ADDR" \
		-put-content-key \
		-put-data "$data" \
		-exit-after-put \
		>/dev/null 2>&1
done

"$BIN" \
	-listen "$TARGET_ADDR" \
	-dial "$SOURCE_ADDR" \
	-replicate \
	-store-dir "$TARGET_DIR" \
	-sync-interval 0s \
	-max-inventory-bytes 128 \
	-max-inventory-keys 1 \
	-health "$TARGET_HEALTH" \
	>"$TARGET_LOG" 2>&1 &
target_pid=$!
wait_ready target "http://$TARGET_HEALTH" "$TARGET_LOG"

wait_metric_at_least source "http://$SOURCE_HEALTH" replication_inventory_exchanges_limited 1 "$SOURCE_LOG"
wait_metric_at_least source "http://$SOURCE_HEALTH" replication_inventory_exchanges_active 1 "$SOURCE_LOG"

kill "$target_pid" 2>/dev/null || true
wait "$target_pid" 2>/dev/null || true
target_pid=""
wait_metric_value source "http://$SOURCE_HEALTH" replication_inventory_exchanges_active 0 "$SOURCE_LOG"

"$BIN" \
	-listen "$TARGET_ADDR" \
	-dial "$SOURCE_ADDR" \
	-replicate \
	-store-dir "$TARGET_DIR" \
	-sync-interval 0s \
	-max-inventory-bytes 128 \
	-max-inventory-keys 1 \
	-health "$TARGET_HEALTH" \
	>"$TARGET_LOG" 2>&1 &
target_pid=$!
wait_ready target "http://$TARGET_HEALTH" "$TARGET_LOG"
wait_keys target "$TARGET_DIR" "$TARGET_LOG"
wait_metric_at_least source "http://$SOURCE_HEALTH" replication_inventory_exchanges_started 2 "$SOURCE_LOG"
wait_metric_at_least source "http://$SOURCE_HEALTH" replication_inventory_exchanges_completed 1 "$SOURCE_LOG"
wait_metric_at_least source "http://$SOURCE_HEALTH" replication_inventory_keys_sent 8 "$SOURCE_LOG"
wait_metric_value source "http://$SOURCE_HEALTH" replication_inventory_exchanges_active 0 "$SOURCE_LOG"

prometheus="$(curl -fsS "http://$SOURCE_HEALTH/metrics/prometheus")"
printf '%s\n' "$prometheus" | grep '^streamhive_replication_inventory_exchanges_limited [1-9]' >/dev/null
printf '%s\n' "$prometheus" | grep '^streamhive_replication_inventory_exchanges_completed [1-9]' >/dev/null
printf '%s\n' "$prometheus" | grep '^streamhive_replication_inventory_keys_sent [1-9]' >/dev/null
printf '%s\n' "$prometheus" | grep '^streamhive_replication_inventory_exchanges_active 0$' >/dev/null

echo "budgeted inventory demo passed: 8 content-addressed blobs converged after target restart"
echo "source metrics:"
curl -fsS "http://$SOURCE_HEALTH/metrics"
echo "target metrics:"
curl -fsS "http://$TARGET_HEALTH/metrics"
