#!/bin/sh
# Build each crypto dependency independently with the native userspace target.
set -eu

work=${1:?output directory required}
opt=${OPT:-2}
tags=${BUILD_TAGS:-goos_hz100 goos_validation}
mkdir -p "$work/elf" "$work/images" "$work/logs"
make --no-print-directory toolchain-check build/userspace/runtime.o
if [ "${CRYPTO_COMPAT:-0}" = 1 ]; then
	original_root=$(tinygo env TINYGOROOT)
	mkdir "$work/tinygo"
	go run ./tools/cryptocompat "$original_root" "$work/tinygo" tinygo/runtime/native.go.txt
	export TINYGOROOT="$work/tinygo"
fi
{
	go version
	tinygo version
	printf 'target=tinygo/targets/goos-userspace-amd64.json opt=%s tags=%s\n' "$opt" "$tags"
	shasum -a 256 tools/toolchain.versions tinygo/targets/goos-userspace-amd64.json
	tinygo_root=$(tinygo env TINYGOROOT)
	for file in crypto/tls/tls.go crypto/rand/rand.go; do
		shasum -a 256 "$tinygo_root/src/$file"
	done
} >"$work/toolchain.txt"
printf 'package\tbuild\timage_bytes\n' >"$work/build.tsv"
failed=0
for package in platform sha256 sha512 hmac aes cipher ecdh ecdsa rsa rand asn1 pem x509 tls; do
	selected=$tags
	if [ "$package" != platform ]; then
		selected="$selected cryptoprobe_$package"
	fi
	log=$work/logs/$package.log
	elf=$work/elf/$package.elf
	image=$work/images/$package
	rm -f "$elf" "$image"
	printf 'crypto-check: %s\n' "$package"
	if tinygo build -tags "$selected" -target=tinygo/targets/goos-userspace-amd64.json -opt="$opt" -no-debug -o "$elf" ./userspace/bin/cryptoprobe >"$log" 2>&1 &&
		go run ./tools/stackframes "$elf" >>"$log" 2>&1 &&
		go run ./tools/elf2native "$elf" "$image" >>"$log" 2>&1; then
		bytes=$(wc -c <"$image" | tr -d ' ')
		printf '%s\tpass\t%s\n' "$package" "$bytes" >>"$work/build.tsv"
	else
		rm -f "$image"
		printf '%s\tfail\t-\n' "$package" >>"$work/build.tsv"
		printf 'crypto-check: %s unsupported; see %s\n' "$package" "$log" >&2
		tail -n 12 "$log" >&2
		failed=1
	fi
done
cat "$work/build.tsv"
printf 'crypto-check report: %s\n' "$work"
exit "$failed"
