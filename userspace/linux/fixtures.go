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
const MuslHelloSHA256 = "359a65bf92fa92e2a5a213976d3bbe73ba90775037c2360df0e424956ecf1300"
