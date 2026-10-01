// Package linuxfixtures holds the prebuilt static Linux programs the system
// image carries under /system/linux. build.sh rebuilds them from the sources
// beside this file; README.md records the toolchain and the checksums.
package linuxfixtures

import _ "embed"

// Fixture is one prebuilt program and the SHA-256 that pins it. A rebuild
// that changes the binary must change the checksum here and in README.md in
// the same commit.
type Fixture struct {
	Name   string
	Image  []byte
	SHA256 string
}

var (
	//go:embed prebuilt/hello
	hello []byte
	//go:embed prebuilt/hello-pie
	helloPIE []byte
	//go:embed prebuilt/musl-hello
	muslHello []byte
	//go:embed prebuilt/fsprobe
	fsprobe []byte
	//go:embed prebuilt/dirprobe
	dirprobe []byte
	//go:embed prebuilt/abi-probe
	abiProbe []byte
)

// All lists the fixtures in the order the image stores them.
func All() []Fixture {
	return []Fixture{
		{"hello", hello, "9c83f5b9fd8af078d35eff4b2fd852eefff421c25e2cb3eddc14ebea9d37a708"},
		{"hello-pie", helloPIE, "81d29ff66f43da7eb92f9be2509930b1758fda0e97410e63d0881e29c6a5b150"},
		{"musl-hello", muslHello, "0219392433258d1b91944fdaf4d8ee5b1bad7d6eb9ad7871bc8cc4efd7efbf5f"},
		{"fsprobe", fsprobe, "7e1b34187f159cbf673d3a95c2ae366aba88f0aae6ad88fd284f51d1e24b3cc6"},
		{"dirprobe", dirprobe, "6d4767fffedf1e8d820fce56ea791aa813721e1552cd93cdedf4c4151e54ddca"},
		{"abi-probe", abiProbe, "fc60417904816277e21b286d66d3a361f85957fd1fc301468ab74a980314dbb0"},
	}
}
