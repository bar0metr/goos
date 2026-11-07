#!/bin/sh
set -eu

work=${1:?output directory required}
root=${COMPAT_ROOT:?compatibility root required}

[ -d "$root/tinygo" ] || {
	printf 'missing crypto compatibility root: %s\n' "$root" >&2
	exit 2
}

CRYPTO_COMPAT=1 CRYPTO_PACKAGES='sha256 ecdsa' \
	sh tools/crypto-check.sh "$work"
printf 'crypto frame report: %s\n' "$work"
