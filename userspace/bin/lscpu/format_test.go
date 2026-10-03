package main

import (
	"goos/internal/cpuinfo"
	"strings"
	"testing"
)

func fixture() cpuData {
	var data cpuData
	data.summary = cpuinfo.Summary{Validity: cpuinfo.ValidIdentity | cpuinfo.ValidTopology | cpuinfo.ValidAddressWidths | cpuinfo.ValidHypervisor, PresentLogicalCPUs: 4, OnlineCPUs: 1, Family: 6, Model: 85, Stepping: 7, ThreadsPerCore: 2, CoresPerSocket: 2, Sockets: 1, LogicalCPUsPerPackage: 4, PhysicalAddressBits: 46, LinearAddressBits: 48, OperatingModes: cpuinfo.Mode32 | cpuinfo.Mode64, ByteOrder: cpuinfo.ByteOrderLittle}
	copy(data.summary.Vendor[:], "GenuineIntel")
	copy(data.summary.Brand[:], "Example CPU")
	copy(data.summary.HypervisorVendor[:], "KVMKVMKVM")
	data.summary.UsableFeatures.Add(cpuinfo.FeatureSSE2)
	data.entryCount = 4
	for index := 0; index < 4; index++ {
		data.entries[index] = cpuinfo.CPUEntry{LogicalID: uint32(index), APICID: uint32(index), Flags: cpuinfo.CPUEntryPresent | cpuinfo.CPUEntryTopologyValid, CoreID: uint32(index / 2), ThreadID: uint32(index % 2)}
	}
	data.entries[0].Flags |= cpuinfo.CPUEntryOnline | cpuinfo.CPUEntryBootstrap
	data.cacheCount = 1
	data.caches[0] = cpuinfo.CacheInfo{Validity: cpuinfo.CacheGeometryValid, Level: 2, Type: cpuinfo.CacheUnified, LineSize: 64, Ways: 8, Sets: 1024, SharingCount: 2, Size: 512 << 10}
	return data
}

func TestSummaryFormatting(t *testing.T) {
	got := string(format(fixture(), defaultOptions(), nil))
	for _, want := range []string{"Architecture:             x86_64", "CPU(s):                   4", "On-line CPU(s) list:      0", "Off-line CPU(s) list:     1-3", "Hypervisor vendor:        KVM", "L2 cache:                 1 MiB", "Flags:                    sse2"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
func TestJSONEscapesText(t *testing.T) {
	data := fixture()
	copy(data.summary.Brand[:], "bad\"cpu\n")
	got := string(format(data, options{json: true}, nil))
	if !strings.Contains(got, `bad\"cpu\n`) {
		t.Fatalf("JSON: %s", got)
	}
}
func TestJSONHonorsBytesAndHex(t *testing.T) {
	got := string(format(fixture(), options{json: true, bytes: true, hex: true}, nil))
	for _, want := range []string{
		`"field":"On-line CPU(s) mask:","data":"1"`,
		`"field":"Off-line CPU(s) mask:","data":"e"`,
		`"field":"L2 cache:","data":"1048576"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in JSON: %s", want, got)
		}
	}
}
func TestTablesAndUnknownTopology(t *testing.T) {
	data := fixture()
	data.entries[1].Flags &^= cpuinfo.CPUEntryTopologyValid
	option := defaultOptions()
	option.mode = modeExtended
	got := string(format(data, option, nil))
	if !strings.Contains(got, "CPU  ONLINE") || !strings.Contains(got, "1  no  -  -  -  1") {
		t.Fatalf("extended:\n%s", got)
	}
	option.mode = modeParse
	option.cpuColumns = columnCPU | columnOnline | columnAPICID
	got = string(format(data, option, nil))
	if !strings.Contains(got, "#CPU,ONLINE,APICID\n0,yes,0") {
		t.Fatalf("parse:\n%s", got)
	}
}
func TestRawTableFormatting(t *testing.T) {
	option := defaultOptions()
	option.mode, option.raw = modeExtended, true
	got := string(format(fixture(), option, nil))
	if !strings.Contains(got, "CPU ONLINE SOCKET CORE THREAD APICID\n") || strings.Contains(got, "CPU  ONLINE") {
		t.Fatalf("raw extended:\n%s", got)
	}
}
func TestOptions(t *testing.T) {
	option := defaultOptions()
	for _, argument := range []string{"-e=CPU,ONLINE,ACPIUID", "-c", "--output-all"} {
		if problem := option.parse([]byte(argument)); problem != "" {
			t.Fatal(problem)
		}
	}
	if problem := option.validate(); problem != "" {
		t.Fatal(problem)
	}
	if option.mode != modeExtended || option.filter != filterOffline || option.cpuColumns != allCPUColumns {
		t.Fatalf("options: %+v", option)
	}
	bad := defaultOptions()
	bad.parse([]byte("-a"))
	if bad.parse([]byte("-b")) == "" {
		t.Fatal("accepted conflicting filters")
	}
}
func TestOptionsRejectIgnoredCombinations(t *testing.T) {
	for _, arguments := range [][]string{
		{"--raw"},
		{"--extended", "--hex"},
		{"--physical"},
		{"--output-all"},
	} {
		option := defaultOptions()
		for _, argument := range arguments {
			if problem := option.parse([]byte(argument)); problem != "" {
				t.Fatalf("parse %v: %s", arguments, problem)
			}
		}
		if problem := option.validate(); problem == "" {
			t.Fatalf("accepted ignored combination %v", arguments)
		}
	}
}
func TestCacheSummaryDoesNotOverflow(t *testing.T) {
	data := fixture()
	data.caches[0].Size = ^uint64(0)
	option := defaultOptions()
	option.bytes = true
	got := string(format(data, option, nil))
	if !strings.Contains(got, "L2 cache:                 18446744073709551615") {
		t.Fatalf("overflowed cache size:\n%s", got)
	}
}
func TestCPUListAndHexMask(t *testing.T) {
	data := fixture()
	out := output{}
	appendCPUList(&out, data, false)
	if string(out.data) != "1-3" {
		t.Fatalf("list %q", out.data)
	}
	out = output{}
	appendMask(&out, data, false)
	if string(out.data) != "e" {
		t.Fatalf("mask %q", out.data)
	}
}
