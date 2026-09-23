#!/bin/sh
# Checks every external tool the build, `make run`, and the tests use against
# the pins in tools/toolchain.versions (ADR 0086). It only reports: nothing is
# installed, downloaded, or changed. A missing tool or a mismatch of an
# `error` pin exits non-zero; a mismatch of a `warn` pin is reported only.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
versions=$root/tools/toolchain.versions
ovmf=${OVMF_CODE:-/opt/local/share/qemu/edk2-x86_64-code.fd}
# Never let the go command fetch a newer toolchain while being inspected.
export GOTOOLCHAIN=local

case $(uname -m) in
x86_64) goarch=amd64 ;;
*) goarch=arm64 ;;
esac

errors=0
warnings=0

ok() { printf '[ OK ] %s\n' "$1"; }

# report STATUS MESSAGE HINT... prints a problem and its indented hints.
report() {
	status=$1 message=$2
	shift 2
	printf '[%s] %s\n' "$status" "$message" >&2
	for hint in "$@"; do
		printf '       %s\n' "$hint" >&2
	done
}
miss() {
	report MISS "$@"
	errors=$((errors + 1))
}
warn() {
	report WARN "$@"
	warnings=$((warnings + 1))
}

pin() { awk -v tool="$1" -v field="$2" '$1 == tool { print $field; exit }' "$versions"; }

# check NAME INSTALLED HINT... compares an installed version with its pin.
check() {
	name=$1 have=$2
	shift 2
	want=$(pin "$name" 2)
	policy=$(pin "$name" 3)
	if [ -z "$want" ]; then
		report FAIL "$name $have: no pin in tools/toolchain.versions"
		errors=$((errors + 1))
	elif [ "$have" = "$want" ]; then
		ok "$name $have"
	elif [ "$policy" = error ]; then
		report FAIL "$name $have, pinned $want (tools/toolchain.versions)" \
			"$name decides the produced binaries; a different version is not supported." "$@"
		errors=$((errors + 1))
	else
		warn "$name $have, pinned $want (accepted; the build was validated with $want)"
	fi
}

xcode_hint='macOS: xcode-select --install   (Xcode Command Line Tools)'
# /usr/bin/clang, make, and nm are stubs that offer to install the Command
# Line Tools when those are absent; they are not run in that case.
developer=1
if [ "$(uname -s)" = Darwin ] && ! xcode-select -p >/dev/null 2>&1; then
	developer=0
fi
found() {
	command -v "$1" >/dev/null 2>&1 || return 1
	case $(command -v "$1") in
	/usr/bin/*) [ "$developer" -eq 1 ] || [ "$1" = file ] || [ "$1" = nc ] || [ "$1" = shasum ] ;;
	esac
}

if found go; then
	have=$(go env GOVERSION 2>/dev/null | sed 's/^go//')
	check go "$have" \
		"macOS: install https://go.dev/dl/go$(pin go 2).darwin-$goarch.pkg" \
		"or: sudo port install go / brew install go, only if it provides $(pin go 2)"
	module=$(awk '$1 == "go" { print $2; exit }' "$root/go.mod")
	case $(pin go 2). in
	"$module".*) ok "go.mod go $module matches the pinned go" ;;
	*)
		report FAIL "go.mod says go $module but the pinned go is $(pin go 2)" \
			"Update tools/toolchain.versions and go.mod together."
		errors=$((errors + 1))
		;;
	esac
else
	miss go "macOS: install https://go.dev/dl/go$(pin go 2).darwin-$goarch.pkg" \
		"or: sudo port install go / brew install go, only if it provides $(pin go 2)"
fi

tinygo_url="https://github.com/tinygo-org/tinygo/releases/download/v$(pin tinygo 2)/tinygo$(pin tinygo 2).darwin-$goarch.tar.gz"
if found tinygo; then
	check tinygo "$(tinygo version | awk '{ print $3 }')" \
		"macOS: unpack $tinygo_url into /opt and put /opt/tinygo/bin on PATH"
else
	miss tinygo "macOS: unpack $tinygo_url into /opt and put /opt/tinygo/bin on PATH" \
		"or: brew tap tinygo-org/tools && brew install tinygo, only if it provides $(pin tinygo 2)"
fi

if found clang; then
	check clang "$(clang --version | sed -n '1s/.*clang version \([0-9.]*\).*/\1/p')"
else
	miss clang "$xcode_hint" "(assembles kernel/arch/amd64/io.S and userspace/runtime/amd64.S)"
fi

if found make; then
	check make "$(make --version | awk 'NR == 1 { print $3 }')"
else
	miss make "$xcode_hint"
fi

qemu_port='macOS: sudo port install qemu   (MacPorts; firmware in /opt/local/share/qemu, the Makefile default)'
qemu_brew='or: brew install qemu           (then pass OVMF_CODE=$(brew --prefix)/share/qemu/edk2-x86_64-code.fd to make)'
if found qemu-system-x86_64; then
	check qemu-system-x86_64 "$(qemu-system-x86_64 --version | awk 'NR == 1 { print $4 }')"
else
	miss qemu-system-x86_64 "$qemu_port" "$qemu_brew"
fi

if [ -f "$ovmf" ]; then
	sum=$(shasum -a 256 "$ovmf" | awk '{ print $1 }')
	if [ "$sum" = "$(pin ovmf 2)" ]; then
		ok "OVMF firmware: $ovmf"
	else
		warn "OVMF firmware: $ovmf differs from the pinned build (sha256 $sum)" \
			"Accepted; QEMU runs were validated with the firmware of qemu $(pin qemu-system-x86_64 2)."
	fi
else
	set -- "$qemu_port" "$qemu_brew"
	for candidate in /opt/local/share/qemu/edk2-x86_64-code.fd /opt/homebrew/share/qemu/edk2-x86_64-code.fd /usr/local/share/qemu/edk2-x86_64-code.fd; do
		if [ -f "$candidate" ] && [ "$candidate" != "$ovmf" ]; then
			set -- "found $candidate: run make with OVMF_CODE=$candidate"
		fi
	done
	miss "OVMF firmware: $ovmf" "$@"
fi

# nm only writes the kernel symbol map (build/.../goos.sym); the build goes on
# without it.
if found nm; then
	ok nm
else
	warn "nm: the kernel symbol map will be empty" "$xcode_hint"
fi

# Part of the macOS base system; the integration, boot, and reproducibility
# tests use them.
for tool in file nc shasum; do
	if found "$tool"; then
		ok "$tool"
	else
		miss "$tool" "part of the macOS base system; check PATH"
	fi
done

if [ "$errors" -ne 0 ]; then
	printf 'Toolchain check failed: %d error(s), %d warning(s). Nothing was installed.\n' "$errors" "$warnings" >&2
	exit 1
fi
if [ "$warnings" -ne 0 ]; then
	printf 'GoOS toolchain is ready with %d warning(s).\n' "$warnings"
else
	printf 'GoOS toolchain is ready.\n'
fi
