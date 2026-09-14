#!/bin/sh
set -eu

kernel=${1:?kernel path required}
test -s "$kernel"
file "$kernel" | grep -q 'EFI application'
printf 'kernel artifact valid: %s\n' "$kernel"
