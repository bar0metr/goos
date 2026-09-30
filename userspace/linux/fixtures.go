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
const MuslHelloSHA256 = "0aaad813f067c758fee26c4ba20603eb37e17757fb6ef8df01f644552f5a4780"
