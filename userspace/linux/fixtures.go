// Package linuxfixtures holds the prebuilt static Linux programs the
// validation image carries. build.sh rebuilds them from the C sources beside
// this file; README.md records the toolchain and the checksums.
package linuxfixtures

import _ "embed"

// MuslHello is musl-hello/main.c linked statically against musl.
//
//go:embed prebuilt/musl-hello
var MuslHello []byte

// MuslHelloSHA256 pins MuslHello. A rebuild that changes the binary must
// change this value and README.md in the same commit.
const MuslHelloSHA256 = "442a6f1ddd0b5b0f6cf1c6ae69398d1f2f308df86c6dff9f658f78e32ee66768"
