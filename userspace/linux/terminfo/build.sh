#!/bin/sh
# Rebuilds userspace/linux/terminfo/db, the compiled terminfo tree that a
# static ncurses program (GNU nano) reads on GoOS, from goos.src and three
# stock ncurses entries (xterm, xterm-256color, vt100).
#
# tic runs in a pinned Alpine container, so the result does not depend on the
# host's ncurses. The tree uses the directory layout ncurses expects:
# db/<first letter>/<name>, for example db/g/goos and db/x/xterm.
#
# Every entry is written in the legacy format (magic 0432, 16-bit numbers).
# The ncurses inside the static nano also reads the 32-bit format of
# ncurses 6.1 and later (it loads Alpine's xterm-256color, magic 01036), but
# one format for the whole tree keeps it readable by any ncurses 5 or 6
# build: xterm-256color is cut from pairs#65536 to pairs#32767 so it fits,
# and extended (user-defined) capabilities are dropped since tic runs
# without -x.
#
# usage: userspace/linux/terminfo/build.sh
set -eu

here=$(cd "$(dirname "$0")" && pwd)
image=alpine:3.22

rm -rf "$here/db"
mkdir -p "$here/db"
docker run --rm -i --platform linux/amd64 "$image" sh -c '
	set -e
	apk add --no-cache -q ncurses ncurses-terminfo-base >&2
	mkdir -p /db
	cat > /goos.src
	for name in xterm xterm-256color vt100; do
		infocmp -1 -I "$name" |
			sed "s/pairs#0x10000/pairs#32767/; s/pairs#65536/pairs#32767/"
	done > /stock.src
	tic -o /db /goos.src
	tic -o /db /stock.src
	# tic links aliases such as vt100-am; the GoOS disk has no symlinks.
	find /db -type l -delete
	TERMINFO=/db infocmp -1 goos >&2
	find /db -type f | sort >&2
	tar -C /db -cf - .
' < "$here/goos.src" | tar -C "$here/db" -xf -
find "$here/db" -type f | sort
