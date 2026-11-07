#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cache=${GOOS_BUILD_CACHE:-$root/build/cache}
mkdir -p "$cache"
cache=$(CDPATH= cd -- "$cache" && pwd)
namespace=${TINYGO_CACHE_NAMESPACE:-}
case $namespace in
  *[!a-zA-Z0-9_-]*) echo "invalid TinyGo cache namespace: $namespace" >&2; exit 1 ;;
esac
home=$cache/tinygo-home${namespace:+-$namespace}

if [ "$(uname -s)" = Darwin ]; then
  if command -v go >/dev/null 2>&1; then
    GOPATH=$(go env GOPATH)
    GOMODCACHE=$(go env GOMODCACHE)
    if [ "${GOENV:-}" != off ]; then
      GOENV=$(go env GOENV)
    fi
    export GOPATH GOMODCACHE GOENV
  else
    GOPATH=${GOPATH:-${HOME:?}/go}
    GOMODCACHE=${GOMODCACHE:-${GOPATH%%:*}/pkg/mod}
    export GOPATH GOMODCACHE
  fi
  mkdir -p "$home/Library/Caches"
  if [ -z "${NETRC:-}" ] && [ -n "${HOME:-}" ]; then
    NETRC=$HOME/.netrc
    export NETRC
  fi
  HOME=$home
  export HOME
else
  mkdir -p "$home/cache"
  XDG_CACHE_HOME=$home/cache
  export XDG_CACHE_HOME
fi
exec "${TINYGO_BIN:-tinygo}" "$@"
