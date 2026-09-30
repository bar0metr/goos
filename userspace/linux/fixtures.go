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
const MuslHelloSHA256 = "7b843cd21f11936db53ce1e51985dfa0853c1276a6bf9ed48a46f2424fcb206c"
