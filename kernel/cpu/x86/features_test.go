package x86

import (
	"goos/internal/cpuinfo"
	"testing"
)

func TestDetectFeaturesMapsLeavesAndFiltersExtendedState(t *testing.T) {
	input := source(map[leaf]Registers{
		{1, 0}:          {ECX: 1<<12 | 1<<26 | 1<<27 | 1<<28 | 1<<29 | 1<<30, EDX: 1<<25 | 1<<26},
		{7, 0}:          {EBX: 1<<0 | 1<<5 | 1<<16 | 1<<18, ECX: 1<<3 | 1<<8 | 1<<9 | 1<<22, EDX: 1 << 14},
		{0x80000001, 0}: {ECX: 1<<2 | 1<<5, EDX: 1<<20 | 1<<29},
	})
	hardware, usable := DetectFeatures(input, 7, 0x80000001)
	for _, feature := range []cpuinfo.Feature{
		cpuinfo.FeatureSSE, cpuinfo.FeatureSSE2, cpuinfo.FeatureFMA, cpuinfo.FeatureAVX,
		cpuinfo.FeatureF16C, cpuinfo.FeatureRDRAND, cpuinfo.FeatureAVX2,
		cpuinfo.FeatureAVX512F, cpuinfo.FeatureRDSEED, cpuinfo.FeatureSVM,
		cpuinfo.FeatureABM, cpuinfo.FeatureNX, cpuinfo.FeatureLM, cpuinfo.FeatureVAES,
		cpuinfo.FeatureRDPID, cpuinfo.FeatureSERIALIZE, cpuinfo.FeatureXSAVE,
		cpuinfo.FeatureOSXSAVE, cpuinfo.FeatureFSGSBASE, cpuinfo.FeatureGFNI,
		cpuinfo.FeaturePKU,
	} {
		if !hardware.Has(feature) {
			t.Errorf("hardware missing %s", cpuinfo.FeatureName(feature))
		}
	}
	for _, feature := range []cpuinfo.Feature{
		cpuinfo.FeatureXSAVE, cpuinfo.FeatureOSXSAVE, cpuinfo.FeatureFMA,
		cpuinfo.FeatureAVX, cpuinfo.FeatureF16C, cpuinfo.FeatureFSGSBASE,
		cpuinfo.FeatureAVX2, cpuinfo.FeatureAVX512F, cpuinfo.FeaturePKU, cpuinfo.FeatureGFNI,
		cpuinfo.FeatureVAES,
	} {
		if usable.Has(feature) {
			t.Errorf("stateful feature marked usable: %s", cpuinfo.FeatureName(feature))
		}
	}
	for _, feature := range []cpuinfo.Feature{cpuinfo.FeatureSSE, cpuinfo.FeatureSSE2, cpuinfo.FeatureRDRAND, cpuinfo.FeatureNX} {
		if !usable.Has(feature) {
			t.Errorf("usable feature missing: %s", cpuinfo.FeatureName(feature))
		}
	}
}

func TestDetectFeaturesHonorsMaximumLeaves(t *testing.T) {
	input := source(map[leaf]Registers{
		{7, 0}:          {EBX: ^uint32(0)},
		{0x80000001, 0}: {ECX: ^uint32(0), EDX: ^uint32(0)},
	})
	hardware, _ := DetectFeatures(input, 1, 0x80000000)
	if hardware.Has(cpuinfo.FeatureAVX2) || hardware.Has(cpuinfo.FeatureNX) {
		t.Fatal("accepted features beyond maximum leaves")
	}
	if input.queries[leaf{7, 0}] != 0 || input.queries[leaf{0x80000001, 0}] != 0 {
		t.Fatal("queried leaves beyond advertised maxima")
	}
}

func TestStableFeatureNames(t *testing.T) {
	if cpuinfo.FeatureName(cpuinfo.FeaturePNI) != "pni" ||
		cpuinfo.FeatureName(cpuinfo.FeatureSHA) != "sha_ni" ||
		cpuinfo.FeatureName(cpuinfo.FeatureCount) != "" {
		t.Fatal("feature name contract changed")
	}
}
