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
const MuslHelloSHA256 = "d2d25146c7f6d26f85911e13b078ddb16a4c3d3606362c7c3eba7510f46505fb"
