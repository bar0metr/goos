#!/bin/sh
# Runs stress.sh in parallel batches, each guest with its own disk and log.
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
runs=${3:-4}
jobs=${4:-4}
rounds=${5:-15}
# Each guest boots its own installed disk, made from the build's system tree
# (ADR 0175); the Makefile names the tree.
tree=${SYSTEM_TREE:?SYSTEM_TREE required}
out=build/stress/hz$(cat build/config/build | cut -d' ' -f1)
rm -rf "$out"
mkdir -p "$out"

run=0
failed=0
while [ "$run" -lt "$runs" ]; do
	batch=0
	pids=
	while [ "$batch" -lt "$jobs" ] && [ "$run" -lt "$runs" ]; do
		go run ./tools/mkfat32 -new -tree "$tree" "$out/disk$run.img"
		tests/boot/stress.sh "$iso" "$firmware" "$out/disk$run.img" "$out/run$run.log" "$rounds" >"$out/result$run.txt" 2>&1 &
		pids="$pids $!"
		run=$((run + 1))
		batch=$((batch + 1))
	done
	for pid in $pids; do
		wait "$pid" || failed=$((failed + 1))
	done
done
cat "$out"/result*.txt
printf '%s: %d/%d stress runs passed\n' "$out" $((runs - failed)) "$runs"
[ "$failed" -eq 0 ]
