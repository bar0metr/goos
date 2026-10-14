#!/bin/sh
# Release checks that are not a test of their own (make release-check):
#
#   - the release in internal/version/version.go is the one the normal ISO
#     reports, and it has release notes;
#   - the normal build carries no validation-only code: no validation marker
#     and no fault-injection report in any normal image (ADR 0058), and no
#     identity-authorized console call anywhere (ADR 0094);
#   - the normal archive holds the recovery set and nothing installed on the
#     volume: no validation probe, Linux program, or distribution command
#     (ADR 0175);
#   - the root license and copyright notice declare GPL-3.0-only and the
#     release author;
#   - every generated file is under build/: nothing outside it is untracked
#     or ignored.
#
# Usage: check.sh NORMAL_VARIANT_DIR ISO
set -eu

variant=${1:?normal build variant directory required}
iso=${2:?ISO required}
status=0
complain() {
	printf 'release: %s\n' "$1" >&2
	status=1
}

release=$(sed -n 's/^[[:space:]]*Release[[:space:]]*=[[:space:]]*"\(.*\)"/\1/p' internal/version/version.go)
[ -n "$release" ] || complain 'no release in internal/version/version.go'
[ -f "docs/releases/$release.md" ] || complain "no release notes for $release"
grep -a -q "GoOS $release" "$variant/kernel/goos.efi" || complain "the kernel does not report $release"
[ -s "$iso" ] || complain "no ISO at $iso"
grep -q '^GNU GENERAL PUBLIC LICENSE$' LICENSE || complain 'LICENSE is not GNU GPL'
grep -q '^Version 3, 29 June 2007$' LICENSE || complain 'LICENSE is not GPL version 3'
grep -q '^The SPDX identifier is GPL-3.0-only\.' NOTICE || complain 'NOTICE does not declare GPL-3.0-only'
grep -q '^Copyright (C) 2026 Nikita Parfenovich (aka bar0metr)$' NOTICE || complain 'NOTICE has the wrong copyright holder'

for image in "$variant/kernel/goos.efi" "$variant"/userspace/* "$variant"/userspace/bin/*; do
	[ -f "$image" ] || continue
	case "$image" in *.elf) continue ;; esac
	marker=$(strings -a "$image" | grep -E '^[A-Z0-9]{2,}_[A-Z0-9_]*(_OK|_INJECTED)$' | sed -n '1p' || true)
	[ -z "$marker" ] || complain "validation marker $marker in the normal image $image"
done
archive=$variant/initramfs/initramfs.tar
if [ -f "$archive" ]; then
	members=$(tar -tf "$archive")
	for excluded in system/fault-probe system/linux/ system/force-recovery system/config system/log system/bin/gfm system/bin/bench system/bin/uname system/bin/ps; do
		printf '%s\n' "$members" | grep -qx "$excluded" && complain "$excluded is in the normal archive"
	done
	for required in system/init system/shell system/tty system/commands system/bin/fsck-fat system/bin/edit; do
		printf '%s\n' "$members" | grep -qx "$required" || complain "$required is missing from the normal archive"
	done
else
	complain "no normal archive at $archive"
fi
if grep -R -n 'goos_debug_write' userspace kernel >/dev/null 2>&1; then
	complain 'the retired DebugWrite stub is still present'
fi

if ! go run ./tools/truststore -check security/ca-source.pem security/ca-certificates.pem; then
	complain 'production trust artifact does not match the pinned Mozilla source'
fi
system=$variant/system
if [ ! -d "$system" ]; then
	complain 'normal installed system tree is missing'
else
	for probe in cryptoprobe trustprobe; do
		[ ! -e "$system/system/bin/$probe" ] || complain "validation command $probe is in the normal system tree"
	done
	cmp -s security/ca-certificates.pem "$system/system/security/ca-certificates.pem" || complain 'normal system trust differs from the pinned production bundle'
	if ! go run ./tools/trustscan "$system" security/trust-update-key.pub tools/trustfixture/testdata/public.key tools/trustfixture/testdata/signing-seed-testonly.raw; then
		complain 'production update key or signing-material exclusion failed'
	fi
	[ -f "$system/system/security/trust-update-url" ] || complain 'immutable trust update URL configuration is missing'
	[ -f "$system/system/bin/trust" ] || complain 'native trust command is missing'
	for key in tools/tlsfixture/testdata/*key-testonly.pem; do
		pattern=$(awk 'length($0) > 40 && $0 !~ /-----/ { print; exit }' "$key")
		[ -n "$pattern" ] || { complain "test key scan has no pattern for $key"; continue; }
		if grep -R -F -q -- "$pattern" "$system"; then
			complain 'test-only private key material is present in the normal system tree'
		fi
	done
fi

# Keep non-ASCII paths unquoted so the build/ prefix check sees their real names.
stray=$(git -c core.quotepath=false ls-files --others --exclude-standard; git -c core.quotepath=false ls-files --others --ignored --exclude-standard | grep -v '^build/' || true)
# Finder's .DS_Store files are host metadata. The root .env is the documented,
# ignored secret store for the optional Proxmox lab. Neither is build output.
stray=$(printf '%s\n' "$stray" | grep -v -e '^$' -e '^\.env$' -e '\(^\|/\)\.DS_Store$' || true)
[ -z "$stray" ] || complain "files outside build/ that are not in the repository: $(printf '%s' "$stray" | tr '\n' ' ')"

[ "$status" -eq 0 ] && printf 'PASS release checks (%s)\n' "$release"
exit $status
