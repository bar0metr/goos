#!/bin/sh
# Checks the partition tests/boot/identity-session.sh copied out of a GoOS
# guest with the pinned e2fsprogs build. The account created the directory
# /alice-made and the file /alice-made/f.txt with UID 70000, GID 80000 and mode
# 0644; debugfs must report exactly that, independent of the GoOS decoders.
set -eu
IMAGE=alpine@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
dir=${1:?directory with alice.img required}
[ -f "$dir/alice.img" ] || { echo "check-identity-session: $dir lacks alice.img" >&2; exit 1; }
command -v docker >/dev/null || { echo "check-identity-session: docker not found; this gate cannot be skipped" >&2; exit 1; }
tar -C "$dir" -cf - alice.img | docker run --rm -i ${DOCKER_FLAGS:-} --platform linux/amd64 "$IMAGE" sh -c '
set -eu
for try in 1 2 3; do
	apk add -q e2fsprogs e2fsprogs-extra >/tmp/apk.log 2>&1 && break
	[ "$try" -lt 3 ] || { echo "apk add e2fsprogs e2fsprogs-extra failed 3 times:" >&2; cat /tmp/apk.log >&2; exit 1; }
	sleep 5
done
mke2fs -V 2>&1 | grep -q "^mke2fs 1.47.2 " || { echo "unexpected e2fsprogs: $(mke2fs -V 2>&1 | head -1)" >&2; exit 1; }
mkdir /w && cd /w && tar xf -
file=$(debugfs -R "stat /alice-made/f.txt" alice.img 2>&1)
printf "%s\n" "$file" | grep -qE "Mode: +0644" || { echo "file mode is not 0644:" >&2; printf "%s\n" "$file" >&2; exit 1; }
printf "%s\n" "$file" | grep -qE "User: +70000" || { echo "file UID is not 70000:" >&2; printf "%s\n" "$file" >&2; exit 1; }
printf "%s\n" "$file" | grep -qE "Group: +80000" || { echo "file GID is not 80000:" >&2; printf "%s\n" "$file" >&2; exit 1; }
dir=$(debugfs -R "stat /alice-made" alice.img 2>&1)
printf "%s\n" "$dir" | grep -qE "User: +70000" || { echo "directory UID is not 70000:" >&2; printf "%s\n" "$dir" >&2; exit 1; }
printf "%s\n" "$dir" | grep -qE "Group: +80000" || { echo "directory GID is not 80000:" >&2; printf "%s\n" "$dir" >&2; exit 1; }
printf "identity-session debugfs: UID 70000 GID 80000 mode 0644 on file and directory\n"
'