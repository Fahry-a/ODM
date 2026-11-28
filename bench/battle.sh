#!/usr/bin/env bash
# Local HTTP battle: ODM (C EVP streaming hash) vs aria2c.
# Usage: SIZE=1073741824 REPS=3 CONNS=4 BENCH_DIR=/home/farhan/odm-bench ./bench/battle.sh
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
LABEL=${LABEL:-odm}
ODM=${ODM:-$ROOT/target/release/odm}
BENCH_DIR=${BENCH_DIR:-/tmp/odm-bench}
SIZE=${SIZE:-1073741824}
REPS=${REPS:-3}
CONNS=${CONNS:-4}
PORT=${PORT:-18080}

mkdir -p "$BENCH_DIR/serve" "$BENCH_DIR/out"
PAYLOAD="$BENCH_DIR/serve/blob.bin"
if [[ ! -f $PAYLOAD || $(stat -c%s "$PAYLOAD") -ne $SIZE ]]; then
    dd if=/dev/zero of="$PAYLOAD" bs=1M count=$((SIZE / 1048576)) status=none
fi
EXPECT=$(sha256sum "$PAYLOAD" | cut -d' ' -f1)

caddy file-server --listen "127.0.0.1:$PORT" --root "$BENCH_DIR/serve" --no-compress \
    >"$BENCH_DIR/caddy.log" 2>&1 &
SERVER_PID=$!
trap 'kill $SERVER_PID 2>/dev/null || true' EXIT
for _ in {1..50}; do
    curl -sf -r 0-0 "http://127.0.0.1:$PORT/blob.bin" -o /dev/null && break
    sleep 0.1
done

URL="http://127.0.0.1:$PORT/blob.bin"
MB=$(awk "BEGIN {printf \"%.0f\", $SIZE/1048576}")

verify() {
    local got
    got=$(sha256sum "$1" | cut -d' ' -f1)
    [[ $got == "$EXPECT" ]] || { echo "BAD HASH $1: $got" >&2; return 1; }
}

elapsed() { awk "BEGIN {printf \"%.3f\", $2 - $1}"; }
mbps() { awk "BEGIN {printf \"%.1f\", $MB / ($2 - $1)}"; }

run_odm() {
    local out="$BENCH_DIR/out/odm.bin"
    rm -f "$out" "$out.odm"
    local t0 t1
    t0=$(date +%s.%N)
    "$ODM" -q -y -c "$CONNS" -s 4M -o odm.bin -d "$BENCH_DIR/out" "$URL" \
        >"$BENCH_DIR/odm.log" 2>&1 || { cat "$BENCH_DIR/odm.log" >&2; return 1; }
    t1=$(date +%s.%N)
    verify "$out"
    echo "$t0 $t1"
}

run_aria2() {
    local out="$BENCH_DIR/out/aria2.bin" extra=()
    [[ ${1:-} == checksum ]] && extra=(--checksum="sha-256=$EXPECT")
    rm -f "$out" "$out.aria2"
    local t0 t1
    t0=$(date +%s.%N)
    aria2c -q --file-allocation=none --allow-overwrite=true --auto-file-renaming=false \
        -x "$CONNS" -s "$CONNS" -k 4M -d "$BENCH_DIR/out" -o aria2.bin \
        "${extra[@]}" "$URL" >"$BENCH_DIR/aria2.log" 2>&1 || { cat "$BENCH_DIR/aria2.log" >&2; return 1; }
    t1=$(date +%s.%N)
    verify "$out"
    echo "$t0 $t1"
}

run_one() {
    if [[ $1 == odm ]]; then run_odm; else run_aria2; fi
}

echo "size=${MB}MiB conns=$CONNS reps=$REPS (local HTTP, hash verified every run)"
printf '%-24s %10s %12s\n' runner seconds MB/s
for label in odm aria2; do
    run_one "$label" >/dev/null  # warmup
    for ((i = 1; i <= REPS; i++)); do
        read -r t0 t1 < <(run_one "$label")
        printf '%-24s %10s %12s\n' "$LABEL-$label#$i" "$(elapsed "$t0" "$t1")" "$(mbps "$t0" "$t1")"
    done
done

# aria2 verifies after download by re-reading the file, not while streaming.
read -r t0 t1 < <(run_aria2 checksum)
printf '%-24s %10s %12s\n' 'aria2+checksum' "$(elapsed "$t0" "$t1")" "$(mbps "$t0" "$t1")"
