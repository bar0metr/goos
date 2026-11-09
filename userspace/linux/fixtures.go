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
	//go:embed prebuilt/tty-probe
	ttyProbe []byte
	//go:embed prebuilt/netprobe
	netprobe []byte
	//go:embed prebuilt/pie-probe
	pieProbe []byte
)

// All lists the fixtures in the order the image stores them.
func All() []Fixture {
	return []Fixture{
		{"hello", hello, "9c83f5b9fd8af078d35eff4b2fd852eefff421c25e2cb3eddc14ebea9d37a708"},
		{"hello-pie", helloPIE, "81d29ff66f43da7eb92f9be2509930b1758fda0e97410e63d0881e29c6a5b150"},
		{"musl-hello", muslHello, "0219392433258d1b91944fdaf4d8ee5b1bad7d6eb9ad7871bc8cc4efd7efbf5f"},
		{"fsprobe", fsprobe, "7e1b34187f159cbf673d3a95c2ae366aba88f0aae6ad88fd284f51d1e24b3cc6"},
		{"dirprobe", dirprobe, "4a068a52296fca685abc2a6236f66006b6040f80335b7a3e67deadb849e9f0e6"},
		{"abi-probe", abiProbe, "9bb8ef8ecd3bf12fdcbba9dc2fd2b799cb65745e41fef4773a6d93c6892c16b3"},
		{"tty-probe", ttyProbe, "5f4db52e6f3a1fb82d45271c6165420198557b520e5fdfbc5f8140d1e693c957"},
		{"netprobe", netprobe, "dca2339b9c61ea9f190613aa58b6289e6166a58463e1a4441bfd8ba661d742cb"},
		{"pie-probe", pieProbe, "4e1822fc5fd6c174726c8751cabb0c298e67adfa46e7a82c9b425732b00c7144"},
	}
}
