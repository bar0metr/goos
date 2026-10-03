package x86

import "goos/internal/cpuinfo"

type featureRegister uint8

const (
	registerEAX featureRegister = iota
	registerEBX
	registerECX
	registerEDX
)

type featureBit struct {
	leaf     uint32
	subleaf  uint32
	register featureRegister
	bit      uint8
	feature  cpuinfo.Feature
}

var featureBits = [...]featureBit{
	{1, 0, registerEDX, 0, cpuinfo.FeatureFPU}, {1, 0, registerEDX, 1, cpuinfo.FeatureVME},
	{1, 0, registerEDX, 2, cpuinfo.FeatureDE}, {1, 0, registerEDX, 3, cpuinfo.FeaturePSE},
	{1, 0, registerEDX, 4, cpuinfo.FeatureTSC}, {1, 0, registerEDX, 5, cpuinfo.FeatureMSR},
	{1, 0, registerEDX, 6, cpuinfo.FeaturePAE}, {1, 0, registerEDX, 7, cpuinfo.FeatureMCE},
	{1, 0, registerEDX, 8, cpuinfo.FeatureCX8}, {1, 0, registerEDX, 9, cpuinfo.FeatureAPIC},
	{1, 0, registerEDX, 11, cpuinfo.FeatureSEP}, {1, 0, registerEDX, 12, cpuinfo.FeatureMTRR},
	{1, 0, registerEDX, 13, cpuinfo.FeaturePGE}, {1, 0, registerEDX, 14, cpuinfo.FeatureMCA},
	{1, 0, registerEDX, 15, cpuinfo.FeatureCMOV}, {1, 0, registerEDX, 16, cpuinfo.FeaturePAT},
	{1, 0, registerEDX, 17, cpuinfo.FeaturePSE36}, {1, 0, registerEDX, 19, cpuinfo.FeatureCLFLUSH},
	{1, 0, registerEDX, 23, cpuinfo.FeatureMMX}, {1, 0, registerEDX, 24, cpuinfo.FeatureFXSR},
	{1, 0, registerEDX, 25, cpuinfo.FeatureSSE}, {1, 0, registerEDX, 26, cpuinfo.FeatureSSE2},
	{1, 0, registerEDX, 28, cpuinfo.FeatureHT},
	{1, 0, registerECX, 0, cpuinfo.FeaturePNI}, {1, 0, registerECX, 1, cpuinfo.FeaturePCLMULQDQ},
	{1, 0, registerECX, 5, cpuinfo.FeatureVMX}, {1, 0, registerECX, 9, cpuinfo.FeatureSSSE3},
	{1, 0, registerECX, 12, cpuinfo.FeatureFMA}, {1, 0, registerECX, 13, cpuinfo.FeatureCX16},
	{1, 0, registerECX, 19, cpuinfo.FeatureSSE41}, {1, 0, registerECX, 20, cpuinfo.FeatureSSE42},
	{1, 0, registerECX, 21, cpuinfo.FeatureX2APIC}, {1, 0, registerECX, 22, cpuinfo.FeatureMOVBE},
	{1, 0, registerECX, 23, cpuinfo.FeaturePOPCNT}, {1, 0, registerECX, 25, cpuinfo.FeatureAES},
	{1, 0, registerECX, 26, cpuinfo.FeatureXSAVE}, {1, 0, registerECX, 27, cpuinfo.FeatureOSXSAVE},
	{1, 0, registerECX, 28, cpuinfo.FeatureAVX}, {1, 0, registerECX, 29, cpuinfo.FeatureF16C},
	{1, 0, registerECX, 30, cpuinfo.FeatureRDRAND}, {1, 0, registerECX, 31, cpuinfo.FeatureHypervisor},
	{7, 0, registerEBX, 0, cpuinfo.FeatureFSGSBASE}, {7, 0, registerEBX, 3, cpuinfo.FeatureBMI1},
	{7, 0, registerEBX, 4, cpuinfo.FeatureHLE}, {7, 0, registerEBX, 5, cpuinfo.FeatureAVX2},
	{7, 0, registerEBX, 7, cpuinfo.FeatureSMEP}, {7, 0, registerEBX, 8, cpuinfo.FeatureBMI2},
	{7, 0, registerEBX, 9, cpuinfo.FeatureERMS}, {7, 0, registerEBX, 10, cpuinfo.FeatureINVPCID},
	{7, 0, registerEBX, 11, cpuinfo.FeatureRTM}, {7, 0, registerEBX, 16, cpuinfo.FeatureAVX512F},
	{7, 0, registerEBX, 18, cpuinfo.FeatureRDSEED}, {7, 0, registerEBX, 19, cpuinfo.FeatureADX},
	{7, 0, registerEBX, 20, cpuinfo.FeatureSMAP}, {7, 0, registerEBX, 23, cpuinfo.FeatureCLFLUSHOPT},
	{7, 0, registerEBX, 24, cpuinfo.FeatureCLWB}, {7, 0, registerEBX, 29, cpuinfo.FeatureSHA},
	{7, 0, registerECX, 2, cpuinfo.FeatureUMIP}, {7, 0, registerECX, 3, cpuinfo.FeaturePKU},
	{7, 0, registerECX, 4, cpuinfo.FeatureOSPKE}, {7, 0, registerECX, 5, cpuinfo.FeatureWAITPKG},
	{7, 0, registerECX, 8, cpuinfo.FeatureGFNI}, {7, 0, registerECX, 9, cpuinfo.FeatureVAES},
	{7, 0, registerECX, 10, cpuinfo.FeatureVPCLMULQDQ}, {7, 0, registerECX, 22, cpuinfo.FeatureRDPID},
	{7, 0, registerEDX, 14, cpuinfo.FeatureSERIALIZE}, {7, 0, registerEDX, 20, cpuinfo.FeatureIBT},
	{0x80000001, 0, registerEDX, 11, cpuinfo.FeatureSYSCALL},
	{0x80000001, 0, registerEDX, 20, cpuinfo.FeatureNX},
	{0x80000001, 0, registerEDX, 26, cpuinfo.FeaturePDPE1GB},
	{0x80000001, 0, registerEDX, 27, cpuinfo.FeatureRDTSCP},
	{0x80000001, 0, registerEDX, 29, cpuinfo.FeatureLM},
	{0x80000001, 0, registerECX, 0, cpuinfo.FeatureLAHFLM},
	{0x80000001, 0, registerECX, 2, cpuinfo.FeatureSVM},
	{0x80000001, 0, registerECX, 5, cpuinfo.FeatureABM},
	{0x80000001, 0, registerECX, 6, cpuinfo.FeatureSSE4A},
	{0x80000001, 0, registerECX, 8, cpuinfo.FeaturePREFETCHW},
	{0x80000001, 0, registerECX, 22, cpuinfo.FeatureTOPOEXT},
}

func DetectFeatures(source Source, maximumBasicLeaf, maximumExtendedLeaf uint32) (cpuinfo.FeatureSet, cpuinfo.FeatureSet) {
	var hardware cpuinfo.FeatureSet
	var lastLeaf, lastSubleaf uint32 = ^uint32(0), ^uint32(0)
	var registers Registers
	for _, descriptor := range featureBits {
		if descriptor.leaf < 0x80000000 {
			if descriptor.leaf > maximumBasicLeaf {
				continue
			}
		} else if descriptor.leaf > maximumExtendedLeaf {
			continue
		}
		if descriptor.leaf != lastLeaf || descriptor.subleaf != lastSubleaf {
			registers = source.Query(descriptor.leaf, descriptor.subleaf)
			lastLeaf, lastSubleaf = descriptor.leaf, descriptor.subleaf
		}
		if registerValue(registers, descriptor.register)&(uint32(1)<<descriptor.bit) != 0 {
			hardware.Add(descriptor.feature)
		}
	}
	usable := hardware
	for _, feature := range [...]cpuinfo.Feature{
		cpuinfo.FeatureFMA, cpuinfo.FeatureAVX, cpuinfo.FeatureF16C,
		cpuinfo.FeatureAVX2, cpuinfo.FeatureAVX512F, cpuinfo.FeatureVAES,
		cpuinfo.FeatureVPCLMULQDQ,
	} {
		usable.Remove(feature)
	}
	return hardware, usable
}

func registerValue(registers Registers, register featureRegister) uint32 {
	switch register {
	case registerEAX:
		return registers.EAX
	case registerEBX:
		return registers.EBX
	case registerECX:
		return registers.ECX
	default:
		return registers.EDX
	}
}
