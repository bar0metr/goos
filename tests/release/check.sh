#!/bin/sh
# Release checks that are not a test of their own (make release-check):
#
#   - the release in internal/version/version.go is the one the normal ISO
#     reports, and it has release notes;
#   - the normal build carries no validation-only code: no validation marker
#     and no fault-injection report in any normal image (ADR 0058), and no
#     identity-authorized console call anywhere (ADR 0094);
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

for image in "$variant/kernel/goos.efi" "$variant"/userspace/* "$variant"/userspace/bin/*; do
	[ -f "$image" ] || continue
	case "$image" in *.elf) continue ;; esac
	marker=$(strings -a "$image" | grep -E '^[A-Z0-9]{2,}_[A-Z0-9_]*(_OK|_INJECTED)$' | sed -n '1p' || true)
	[ -z "$marker" ] || complain "validation marker $marker in the normal image $image"
done
if grep -R -n 'goos_debug_write' userspace kernel >/dev/null 2>&1; then
	complain 'the retired DebugWrite stub is still present'
fi

stray=$(git ls-files --others --exclude-standard; git ls-files --others --ignored --exclude-standard | grep -v -e '^build/' -e '^\.claude/' || true)
# Finder's .DS_Store files are host metadata the repository ignores, not
# build output.
stray=$(printf '%s\n' "$stray" | grep -v -e '^$' -e '^\.claude/' -e '\(^\|/\)\.DS_Store$' || true)
[ -z "$stray" ] || complain "files outside build/ that are not in the repository: $(printf '%s' "$stray" | tr '\n' ' ')"

[ "$status" -eq 0 ] && printf 'PASS release checks (%s)\n' "$release"
exit $status
