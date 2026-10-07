// netpeer is a deterministic peer for the network boot gate. QEMU guests
// connect to its Unix socket with
//
//	-netdev stream,id=net0,server=off,addr.type=unix,addr.path=SOCKET
//
// It floods every frame between the guests like a hub, and it is a station
// on the segment that answers ARP and ICMP echo, serves static DHCP leases
// and an authoritative DNS zone under "test", logs what it sees, and injects
// ARP address conflicts on command.
package main

import (
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

type leaseFlag map[mac]ipv4

func (l leaseFlag) String() string { return "" }

func (l leaseFlag) Set(text string) error {
	hw, addr, ok := strings.Cut(text, "=")
	if !ok {
		return fmt.Errorf("want MAC=IP, got %q", text)
	}
	m, err := parseMAC(hw)
	if err != nil {
		return err
	}
	a, err := parseIPv4Addr(addr)
	if err != nil {
		return err
	}
	l[m] = a
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "netpeer:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("netpeer", flag.ContinueOnError)
	socket := flags.String("socket", "", "Unix socket path QEMU guests connect to (required)")
	hostMAC := flags.String("mac", "52:54:00:00:02:53", "the peer's MAC address")
	hostIP := flags.String("ip", "192.0.2.53/24", "the peer's IPv4 address and prefix length")
	router := flags.String("router", "", "router address offered in DHCP option 3 (omitted when empty)")
	dns := flags.String("dns", "", "comma-separated DNS servers for DHCP option 6 (default: the peer's address)")
	leaseTime := flags.Uint("lease-time", 3600, "DHCP lease time in seconds")
	leases := leaseFlag{}
	flags.Var(leases, "lease", "static lease MAC=IP (repeatable)")
	zonePath := flags.String("zone", "", "DNS zone file")
	noEDNS := flags.Bool("no-edns", false, "answer queries carrying OPT with FORMERR")
	logPath := flags.String("log", "", "event log file (default: standard output)")
	controlPath := flags.String("control", "", "control FIFO or file for conflict commands")
	foreign := flags.String("foreign-mac", "52:54:00:de:ad:01", "MAC address of the conflicting host")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *socket == "" || flags.NArg() != 0 {
		flags.Usage()
		return fmt.Errorf("-socket is required and no arguments are taken")
	}

	var cfg config
	var err error
	if cfg.mac, err = parseMAC(*hostMAC); err != nil {
		return err
	}
	if cfg.foreignMAC, err = parseMAC(*foreign); err != nil {
		return err
	}
	prefix, err := netip.ParsePrefix(*hostIP)
	if err != nil || !prefix.Addr().Is4() {
		return fmt.Errorf("invalid -ip %q, want ADDRESS/PREFIX", *hostIP)
	}
	cfg.ip = prefix.Addr().As4()
	mask := ^uint32(0) << (32 - prefix.Bits())
	if prefix.Bits() == 0 {
		mask = 0
	}
	cfg.mask = ipv4{byte(mask >> 24), byte(mask >> 16), byte(mask >> 8), byte(mask)}
	if *router != "" {
		if cfg.router, err = parseIPv4Addr(*router); err != nil {
			return err
		}
		cfg.hasRouter = true
	}
	if *dns == "" {
		cfg.dns = []ipv4{cfg.ip}
	} else {
		for _, text := range strings.Split(*dns, ",") {
			addr, err := parseIPv4Addr(strings.TrimSpace(text))
			if err != nil {
				return err
			}
			cfg.dns = append(cfg.dns, addr)
		}
	}
	if *leaseTime > 0xffffffff {
		return fmt.Errorf("-lease-time %d is too large", *leaseTime)
	}
	cfg.leaseTime = uint32(*leaseTime)
	cfg.leases = leases
	cfg.noEDNS = *noEDNS
	if *zonePath != "" {
		file, err := os.Open(*zonePath)
		if err != nil {
			return err
		}
		cfg.zone, err = parseZone(file)
		file.Close()
		if err != nil {
			return err
		}
	}

	var out io.Writer = os.Stdout
	if *logPath != "" {
		file, err := os.OpenFile(*logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		defer file.Close()
		out = file
	}
	if *controlPath != "" {
		if err := prepareControl(*controlPath); err != nil {
			return err
		}
	}

	srv, err := listen(*socket, newHost(cfg, &logger{w: out}))
	if err != nil {
		return err
	}
	defer srv.close()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go srv.serve()
	if *controlPath != "" {
		go srv.control(*controlPath)
	}
	fmt.Printf("netpeer: listening %s\n", *socket)
	select {
	case <-signals:
	case <-srv.quit:
	}
	return nil
}
