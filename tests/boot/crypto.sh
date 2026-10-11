#!/bin/sh
# Execute every successfully built crypto probe in one disposable guest.
set -eu
work=${1:?probe output directory required}
iso=${2:?ISO path required}
firmware=${3:?firmware path required}
tree=${4:?system tree required}
log=$work/native.log
pid=
cleanup() {
	if [ -n "$pid" ]; then
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	fi
	exec 3>&- 2>/dev/null || true
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM
wait_for() {
	elapsed=0
	while [ "$elapsed" -lt "$2" ]; do
		tr -d '\r' <"$log" 2>/dev/null | grep -q -- "$1" && return 0
		kill -0 "$pid" 2>/dev/null || break
		sleep 1
		elapsed=$((elapsed+1))
	done
	printf 'crypto: timeout waiting for %s; log %s\n' "$1" "$log" >&2
	tail -n 40 "$log" >&2
	return 1
}
window() {
	awk -v command="$1" '{ sub(/\r$/, "") } seen && index($0, "goos> ") == 1 { exit } seen { print } !seen && ($0 == "goos> " command || $0 == command) { seen = 1 }' "$log"
}

[ ! -e "$work/tree" ] || { echo 'crypto: disposable tree already exists' >&2; exit 2; }
cp -R "$tree" "$work/tree"
go run ./tools/cryptopack "$work/tree" "$work/images"
go run ./tools/mkfat32 -new -tree "$work/tree" "$work/disk.img"
mkfifo "$work/console.in"
qemu-system-x86_64 -machine q35 -smp "${SMP:-1}" -accel tcg,thread=multi -m "${MEMORY:-512M}" \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" -serial stdio -display none -no-reboot -cdrom "$iso" \
	-drive "if=none,id=goosdisk,format=raw,file=$work/disk.img" -device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	<"$work/console.in" >"$log" 2>&1 &
pid=$!
exec 3>"$work/console.in"
wait_for 'goos> ' 90
# Let the validation storage-driver replacement complete before starting probes.
sleep 4
printf 'package\tnative\n' >"$work/native.tsv"
failed=0
for image in "$work/images"/*; do
	[ -f "$image" ] || continue
	package=${image##*/}
	command=crypto-$package
	printf '%s\n' "$command" >&3
	elapsed=0
	while [ "$elapsed" -lt 60 ]; do
		if window "$command" | grep -q '^SHELL_EXEC_OK$'; then break; fi
		kill -0 "$pid" 2>/dev/null || break
		sleep 1
		elapsed=$((elapsed+1))
	done
	if ! window "$command" | grep -q '^SHELL_EXEC_OK$'; then
		printf '%s\ttimeout-or-crash\n' "$package" >>"$work/native.tsv"
		cat "$work/native.tsv"
		tail -n 40 "$log" >&2
		exit 1
	fi
	if window "$command" | grep -q "^CRYPTO_PASS $package$" && ! window "$command" | grep -q 'exit status:'; then
		printf '%s\tpass\n' "$package" >>"$work/native.tsv"
	else
		printf '%s\tfail\n' "$package" >>"$work/native.tsv"
		failed=1
	fi
	done
printf 'poweroff\n' >&3
wait_for '^Powering off\.\.\.$' 30
elapsed=0
while kill -0 "$pid" 2>/dev/null && [ "$elapsed" -lt 30 ]; do
	sleep 1
	elapsed=$((elapsed+1))
done
if kill -0 "$pid" 2>/dev/null; then
	echo 'crypto: guest did not power off' >&2
	exit 1
fi
wait "$pid"
pid=
cat "$work/native.tsv"
grep '^CRYPTO_RESOURCE ' "$log" || true
printf 'crypto native report: %s\n' "$work"
exit "$failed"
