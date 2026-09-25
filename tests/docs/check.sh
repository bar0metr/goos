#!/bin/sh
# Validates the documentation (make docs):
#
#   - every relative Markdown link in README.md and docs/ names a file or
#     directory that exists;
#   - every ADR has a Status and a Decision (ADR 0001 onwards use either a
#     "## Status" heading or a "Status:" line);
#   - the release named in internal/version/version.go has release notes.
set -eu

status=0
complain() {
	printf 'docs: %s\n' "$1" >&2
	status=1
}

for file in README.md $(find docs -name '*.md' | sort); do
	dir=$(dirname "$file")
	# Inline links: ](target), without anchors, schemes, or absolute URLs.
	for target in $(grep -o '](\([^)#]*\)[^)]*)' "$file" | sed 's/^](\([^)#]*\).*/\1/' | sort -u); do
		case "$target" in
		'' | http:* | https:* | mailto:*) continue ;;
		esac
		[ -e "$dir/$target" ] || complain "$file: broken link $target"
	done
done

for adr in docs/adr/[0-9][0-9][0-9][0-9]-*.md; do
	grep -q -e '^## Status' -e '^Status:' "$adr" || complain "$adr: no status"
	grep -q -e '^## Decision' -e '^## Decisions' "$adr" || complain "$adr: no decision"
done

release=$(sed -n 's/^[[:space:]]*Release[[:space:]]*=[[:space:]]*"\(.*\)"/\1/p' internal/version/version.go)
[ -n "$release" ] || complain 'no release in internal/version/version.go'
[ -f "docs/releases/$release.md" ] || complain "no release notes docs/releases/$release.md"

exit $status
