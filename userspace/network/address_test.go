package network

import "testing"

func TestParseIPv4(t *testing.T) {
	for _, test := range []struct {
		text string
		want IPv4
		ok   bool
	}{
		{"10.0.2.15", IPv4{10, 0, 2, 15}, true},
		{"0.0.0.0", IPv4{}, true},
		{"255.255.255.255", IPv4{255, 255, 255, 255}, true},
		{"1.22.133.9", IPv4{1, 22, 133, 9}, true},
		{"", IPv4{}, false},
		{"10.0.2", IPv4{}, false},
		{"10.0.2.15.1", IPv4{}, false},
		{"10.0.2.", IPv4{}, false},
		{".10.0.2", IPv4{}, false},
		{"10..2.15", IPv4{}, false},
		{"256.0.0.1", IPv4{}, false},
		{"1.2.3.1000", IPv4{}, false},
		{"01.2.3.4", IPv4{}, false},
		{"1.2.3.00", IPv4{}, false},
		{"+1.2.3.4", IPv4{}, false},
		{"-1.2.3.4", IPv4{}, false},
		{" 1.2.3.4", IPv4{}, false},
		{"1.2.3.4 ", IPv4{}, false},
		{"1.2.3.4\x00", IPv4{}, false},
		{"0x1.2.3.4", IPv4{}, false},
		{"localhost", IPv4{}, false},
		{"1111.2.3.4", IPv4{}, false},
		{"999.999.999.999", IPv4{}, false},
		{"0000000000000001.1.1.1", IPv4{}, false},
	} {
		got, ok := ParseIPv4([]byte(test.text))
		if ok != test.ok || got != test.want {
			t.Errorf("ParseIPv4(%q) = %v %v, want %v %v", test.text, got, ok, test.want, test.ok)
		}
	}
}

func TestAppendText(t *testing.T) {
	var buffer [MaxIPv4Text]byte
	for _, test := range []struct {
		address IPv4
		want    string
	}{
		{IPv4{10, 0, 2, 15}, "10.0.2.15"},
		{IPv4{255, 255, 255, 255}, "255.255.255.255"},
		{IPv4{0, 100, 9, 109}, "0.100.9.109"},
	} {
		got := AppendIPv4(buffer[:0], test.address)
		if string(got) != test.want || &got[0] != &buffer[0] {
			t.Errorf("AppendIPv4 %v = %q", test.address, got)
		}
		parsed, ok := ParseIPv4(got)
		if !ok || parsed != test.address {
			t.Errorf("round trip %v", test.address)
		}
	}
	var macText [MaxMACText]byte
	if got := AppendMAC(macText[:0], MAC{0x52, 0x54, 0x00, 0xab, 0xcd, 0xef}); string(got) != "52:54:00:ab:cd:ef" {
		t.Errorf("AppendMAC = %q", got)
	}
}

func TestIsUnicast(t *testing.T) {
	for _, test := range []struct {
		address IPv4
		want    bool
	}{
		{IPv4{10, 0, 2, 15}, true},
		{IPv4{192, 0, 2, 1}, true},
		{IPv4{223, 255, 255, 254}, true},
		{IPv4{}, false},
		{IPv4{255, 255, 255, 255}, false},
		{IPv4{224, 0, 0, 1}, false},
		{IPv4{239, 1, 2, 3}, false},
		{IPv4{127, 0, 0, 1}, false},
		{IPv4{240, 0, 0, 1}, false},
	} {
		if got := test.address.IsUnicast(); got != test.want {
			t.Errorf("%v IsUnicast = %v", test.address, got)
		}
	}
}

func TestValidate(t *testing.T) {
	for _, test := range []struct {
		name   string
		config Config
		want   Field
	}{
		{"dhcp ignores the rest", Config{Mode: ModeDHCP, Address: IPv4{255, 1, 1, 1}, Prefix: 99}, FieldNone},
		{"static", staticConfig, FieldNone},
		{"static without gateway", Config{Mode: ModeStatic, Address: hostIP, Prefix: 24}, FieldNone},
		{"slash 30", Config{Mode: ModeStatic, Address: IPv4{192, 0, 2, 1}, Prefix: 30, Gateway: IPv4{192, 0, 2, 2}}, FieldNone},
		{"slash 1", Config{Mode: ModeStatic, Address: IPv4{10, 0, 0, 1}, Prefix: 1}, FieldNone},
		{"unknown mode", Config{Mode: 2, Address: hostIP, Prefix: 24}, FieldMode},
		{"zero address", Config{Mode: ModeStatic, Prefix: 24}, FieldAddress},
		{"broadcast address", Config{Mode: ModeStatic, Address: IPv4{255, 255, 255, 255}, Prefix: 24}, FieldAddress},
		{"multicast address", Config{Mode: ModeStatic, Address: IPv4{224, 0, 0, 5}, Prefix: 24}, FieldAddress},
		{"loopback address", Config{Mode: ModeStatic, Address: IPv4{127, 0, 0, 1}, Prefix: 8}, FieldAddress},
		{"class e address", Config{Mode: ModeStatic, Address: IPv4{250, 0, 0, 1}, Prefix: 8}, FieldAddress},
		{"network address", Config{Mode: ModeStatic, Address: IPv4{10, 0, 2, 0}, Prefix: 24}, FieldAddress},
		{"directed broadcast", Config{Mode: ModeStatic, Address: IPv4{10, 0, 2, 255}, Prefix: 24}, FieldAddress},
		{"prefix zero", Config{Mode: ModeStatic, Address: hostIP, Prefix: 0}, FieldPrefix},
		{"prefix 31", Config{Mode: ModeStatic, Address: hostIP, Prefix: 31}, FieldPrefix},
		{"prefix 32", Config{Mode: ModeStatic, Address: hostIP, Prefix: 32}, FieldPrefix},
		{"gateway outside", Config{Mode: ModeStatic, Address: hostIP, Prefix: 24, Gateway: IPv4{10, 0, 3, 1}}, FieldGateway},
		{"gateway is self", Config{Mode: ModeStatic, Address: hostIP, Prefix: 24, Gateway: hostIP}, FieldGateway},
		{"gateway broadcast", Config{Mode: ModeStatic, Address: hostIP, Prefix: 24, Gateway: IPv4{10, 0, 2, 255}}, FieldGateway},
		{"gateway network", Config{Mode: ModeStatic, Address: hostIP, Prefix: 24, Gateway: IPv4{10, 0, 2, 0}}, FieldGateway},
		{"gateway multicast", Config{Mode: ModeStatic, Address: hostIP, Prefix: 24, Gateway: IPv4{224, 0, 2, 1}}, FieldGateway},
	} {
		if got := test.config.Validate(); got != test.want {
			t.Errorf("%s: Validate = %d, want %d", test.name, got, test.want)
		}
	}
}

func TestParseConfig(t *testing.T) {
	for _, test := range []struct {
		name    string
		mode    string
		address string
		prefix  uint64
		gateway string
		want    Config
		field   Field
	}{
		{"default dhcp", "dhcp", "0.0.0.0", 24, "0.0.0.0", Config{Mode: ModeDHCP}, FieldNone},
		{"dhcp with junk", "dhcp", "junk", 999, "junk", Config{Mode: ModeDHCP}, FieldNone},
		{"static", "static", "10.0.2.15", 24, "10.0.2.2", staticConfig, FieldNone},
		{"static no gateway", "static", "192.0.2.10", 24, "0.0.0.0",
			Config{Mode: ModeStatic, Address: IPv4{192, 0, 2, 10}, Prefix: 24}, FieldNone},
		{"mode case", "DHCP", "0.0.0.0", 24, "0.0.0.0", Config{}, FieldMode},
		{"mode empty", "", "0.0.0.0", 24, "0.0.0.0", Config{}, FieldMode},
		{"default static address", "static", "0.0.0.0", 24, "0.0.0.0", Config{}, FieldAddress},
		{"unparsable address", "static", "10.0.2", 24, "0.0.0.0", Config{}, FieldAddress},
		{"prefix huge", "static", "10.0.2.15", 1 << 40, "0.0.0.0", Config{}, FieldPrefix},
		{"prefix 256", "static", "10.0.2.15", 256, "0.0.0.0", Config{}, FieldPrefix},
		{"prefix 31", "static", "10.0.2.15", 31, "0.0.0.0", Config{}, FieldPrefix},
		{"unparsable gateway", "static", "10.0.2.15", 24, "gateway", Config{}, FieldGateway},
		{"gateway outside", "static", "10.0.2.15", 24, "10.0.9.1", Config{}, FieldGateway},
	} {
		got, field := ParseConfig([]byte(test.mode), []byte(test.address), test.prefix, []byte(test.gateway))
		if got != test.want || field != test.field {
			t.Errorf("%s: ParseConfig = %+v %d, want %+v %d", test.name, got, field, test.want, test.field)
		}
	}
}
