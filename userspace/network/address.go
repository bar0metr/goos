// Package network is the protocol core of the network service: Ethernet, ARP,
// IPv4, ICMP echo, the UDP subset DHCP needs, and the DHCP client. It owns no
// device and no clock. The service passes frames, monotonic time and client
// requests in, and takes frames and echo completions out, so every parser and
// timer here runs on the host.
package network

type IPv4 [4]byte

type MAC [6]byte

type Mode uint8

const (
	ModeDHCP Mode = iota
	ModeStatic
)

type State uint8

const (
	StateDown State = iota
	StateConfiguring
	StateReady
	StateFailed
)

// Field names the configuration value that failed validation.
type Field uint8

const (
	FieldNone Field = iota
	FieldMode
	FieldAddress
	FieldPrefix
	FieldGateway
	// The tuning fields follow in the order of ParseTuning's values.
	FieldTTL
	FieldProbeCount
	FieldProbeWait
	FieldProbeMin
	FieldProbeMax
	FieldAnnounceCount
	FieldAnnounceInterval
	FieldDefendInterval
	FieldMaxConflicts
	FieldConflictRateLimit
	FieldARPCacheTimeout
	FieldARPResolveInterval
	FieldARPResolveAttempts
	FieldEchoReplyRate
	FieldDHCPInitialRetry
	FieldDHCPMaxRetry
	FieldDHCPConflictRetry
	// The UDP limits, system.net.udp.*, in the order of UDPTuning's values.
	// A later transport appends its own block after this one.
	FieldUDPEphemeralMin
	FieldUDPEphemeralMax
	FieldUDPReceiveQueue
	FieldUDPMaxEndpoints
	FieldUDPSessionEndpoints
	FieldUDPMaxPayload
	lastField = FieldUDPMaxPayload
)

// Result values are the GNETAPI1 wire values.
type Result uint8

const (
	ResultOK Result = iota
	ResultDown
	ResultNoAddress
	ResultNoRoute
	ResultNeighborUnreachable
	ResultTimeout
	ResultBusy
	ResultReconfigured
	ResultPermissionDenied
	ResultInvalidArgument
	ResultDriverFault
	ResultConfigInvalid
	ResultRemoteUnreachable
	ResultTimeExceeded
	// ResultAddressInUse is a UDP bind or connect that would make delivery
	// ambiguous. GNETAPI never carries it.
	ResultAddressInUse
)

const (
	MinPrefix = 1
	// MaxPrefix leaves at least two host addresses besides the network and
	// broadcast addresses.
	MaxPrefix = 30
	// MaxIPv4Text is the longest dotted-decimal address, 255.255.255.255.
	MaxIPv4Text = 15
	MaxMACText  = 17
)

var (
	zeroIPv4      = IPv4{}
	broadcastIPv4 = IPv4{255, 255, 255, 255}
	broadcastMAC  = MAC{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
)

// Config is one candidate interface configuration. In DHCP mode the address,
// prefix and gateway are unused and kept zero.
type Config struct {
	Mode    Mode
	Address IPv4
	Prefix  uint8
	Gateway IPv4
}

func (address IPv4) uint32() uint32 {
	return uint32(address[0])<<24 | uint32(address[1])<<16 | uint32(address[2])<<8 | uint32(address[3])
}

func ipv4From(value uint32) IPv4 {
	return IPv4{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)}
}

// IsUnicast reports whether the address can name one host: it is not zero,
// limited broadcast, multicast, loopback or reserved class E.
func (address IPv4) IsUnicast() bool {
	switch {
	case address == zeroIPv4:
		return false
	case address[0] == 127:
		return false
	case address[0] >= 224:
		return false
	}
	return true
}

func prefixMask(prefix uint8) uint32 {
	if prefix == 0 {
		return 0
	}
	if prefix >= 32 {
		return 0xffffffff
	}
	return ^uint32(0) << (32 - prefix)
}

func samePrefix(a, b IPv4, prefix uint8) bool {
	mask := prefixMask(prefix)
	return a.uint32()&mask == b.uint32()&mask
}

// hostAddress reports whether address is a unicast host inside its prefix,
// neither the network nor the directed broadcast address.
func hostAddress(address IPv4, prefix uint8) bool {
	if !address.IsUnicast() || prefix < MinPrefix || prefix > MaxPrefix {
		return false
	}
	host := address.uint32() &^ prefixMask(prefix)
	return host != 0 && host != ^prefixMask(prefix)
}

// Validate returns the first field that makes the configuration unusable.
func (config Config) Validate() Field {
	switch config.Mode {
	case ModeDHCP:
		return FieldNone
	case ModeStatic:
	default:
		return FieldMode
	}
	if !config.Address.IsUnicast() {
		return FieldAddress
	}
	if config.Prefix < MinPrefix || config.Prefix > MaxPrefix {
		return FieldPrefix
	}
	if !hostAddress(config.Address, config.Prefix) {
		return FieldAddress
	}
	if config.Gateway == zeroIPv4 {
		return FieldNone
	}
	if !hostAddress(config.Gateway, config.Prefix) || !samePrefix(config.Gateway, config.Address, config.Prefix) ||
		config.Gateway == config.Address {
		return FieldGateway
	}
	return FieldNone
}

// normalized clears the fields DHCP mode ignores so two DHCP configurations
// always compare equal.
func (config Config) normalized() Config {
	if config.Mode == ModeDHCP {
		return Config{Mode: ModeDHCP}
	}
	return config
}

// ParseConfig builds a configuration from the four stored values. DHCP mode
// ignores the other three, so a stale static address cannot block it.
func ParseConfig(mode, address []byte, prefix uint64, gateway []byte) (Config, Field) {
	var config Config
	switch {
	case equalText(mode, "dhcp"):
		return Config{Mode: ModeDHCP}, FieldNone
	case equalText(mode, "static"):
		config.Mode = ModeStatic
	default:
		return Config{}, FieldMode
	}
	parsed, ok := ParseIPv4(address)
	if !ok {
		return Config{}, FieldAddress
	}
	config.Address = parsed
	if prefix > 255 {
		return Config{}, FieldPrefix
	}
	config.Prefix = uint8(prefix)
	if parsed, ok = ParseIPv4(gateway); !ok {
		return Config{}, FieldGateway
	}
	config.Gateway = parsed
	if field := config.Validate(); field != FieldNone {
		return Config{}, field
	}
	return config, FieldNone
}

func equalText(text []byte, want string) bool {
	if len(text) != len(want) {
		return false
	}
	for index := 0; index < len(want); index++ {
		if text[index] != want[index] {
			return false
		}
	}
	return true
}

// ParseIPv4 accepts strict dotted decimal: four parts of one to three digits,
// each at most 255, with no leading zero except a lone "0". It refuses
// octal-looking and padded forms instead of guessing what they mean.
func ParseIPv4(text []byte) (IPv4, bool) {
	var address IPv4
	if len(text) == 0 || len(text) > MaxIPv4Text {
		return address, false
	}
	part, digits, value := 0, 0, 0
	for index := 0; index <= len(text); index++ {
		if index == len(text) || text[index] == '.' {
			if digits == 0 || part > 3 {
				return IPv4{}, false
			}
			address[part] = byte(value)
			part++
			digits, value = 0, 0
			continue
		}
		character := text[index]
		if character < '0' || character > '9' {
			return IPv4{}, false
		}
		if digits == 1 && value == 0 {
			return IPv4{}, false
		}
		value = value*10 + int(character-'0')
		digits++
		if digits > 3 || value > 255 {
			return IPv4{}, false
		}
	}
	if part != 4 {
		return IPv4{}, false
	}
	return address, true
}

// AppendIPv4 appends dotted decimal. Callers pass a fixed buffer of at least
// MaxIPv4Text bytes so the append never reallocates.
func AppendIPv4(dst []byte, address IPv4) []byte {
	for index, octet := range address[:] {
		if index > 0 {
			dst = append(dst, '.')
		}
		if octet >= 100 {
			dst = append(dst, '0'+octet/100)
		}
		if octet >= 10 {
			dst = append(dst, '0'+octet/10%10)
		}
		dst = append(dst, '0'+octet%10)
	}
	return dst
}

// AppendMAC appends lower-case colon-separated hex, at most MaxMACText bytes.
func AppendMAC(dst []byte, mac MAC) []byte {
	const digits = "0123456789abcdef"
	for index, octet := range mac[:] {
		if index > 0 {
			dst = append(dst, ':')
		}
		dst = append(dst, digits[octet>>4], digits[octet&0x0f])
	}
	return dst
}

func unicastMAC(mac MAC) bool {
	return mac[0]&1 == 0 && mac != MAC{}
}
