// Package cpuinfo defines the stable CPU inventory values shared by the
// kernel and native userspace.
package cpuinfo

// Feature is a stable bit number in the CPU feature bitmap. Values are part of
// the native CPU inventory ABI and must never be renumbered.
type Feature uint16

const (
	FeatureFPU Feature = iota
	FeatureVME
	FeatureDE
	FeaturePSE
	FeatureTSC
	FeatureMSR
	FeaturePAE
	FeatureMCE
	FeatureCX8
	FeatureAPIC
	FeatureSEP
	FeatureMTRR
	FeaturePGE
	FeatureMCA
	FeatureCMOV
	FeaturePAT
	FeaturePSE36
	FeatureCLFLUSH
	FeatureMMX
	FeatureFXSR
	FeatureSSE
	FeatureSSE2
	FeatureHT
	FeaturePNI
	FeaturePCLMULQDQ
	FeatureVMX
	FeatureSSSE3
	FeatureFMA
	FeatureCX16
	FeatureSSE41
	FeatureSSE42
	FeatureX2APIC
	FeatureMOVBE
	FeaturePOPCNT
	FeatureAES
	FeatureXSAVE
	FeatureOSXSAVE
	FeatureAVX
	FeatureF16C
	FeatureRDRAND
	FeatureHypervisor
	FeatureFSGSBASE
	FeatureBMI1
	FeatureHLE
	FeatureAVX2
	FeatureSMEP
	FeatureBMI2
	FeatureERMS
	FeatureINVPCID
	FeatureRTM
	FeatureAVX512F
	FeatureRDSEED
	FeatureADX
	FeatureSMAP
	FeatureCLFLUSHOPT
	FeatureCLWB
	FeatureSHA
	FeatureSYSCALL
	FeatureNX
	FeaturePDPE1GB
	FeatureRDTSCP
	FeatureLM
	FeatureLAHFLM
	FeatureSVM
	FeatureABM
	FeatureSSE4A
	FeaturePREFETCHW
	FeatureTOPOEXT
	FeatureUMIP
	FeaturePKU
	FeatureOSPKE
	FeatureWAITPKG
	FeatureGFNI
	FeatureVAES
	FeatureVPCLMULQDQ
	FeatureRDPID
	FeatureSERIALIZE
	FeatureIBT
	FeatureCount
)

// FeatureSet is the 64-byte feature bitmap used by the CPU inventory ABI.
type FeatureSet [8]uint64

func (set *FeatureSet) Add(feature Feature) {
	if feature < 512 {
		set[feature/64] |= uint64(1) << (feature % 64)
	}
}

func (set FeatureSet) Has(feature Feature) bool {
	return feature < 512 && set[feature/64]&(uint64(1)<<(feature%64)) != 0
}

func (set *FeatureSet) Remove(feature Feature) {
	if feature < 512 {
		set[feature/64] &^= uint64(1) << (feature % 64)
	}
}

var featureNames = [...]string{
	"fpu", "vme", "de", "pse", "tsc", "msr", "pae", "mce", "cx8", "apic",
	"sep", "mtrr", "pge", "mca", "cmov", "pat", "pse36", "clflush", "mmx",
	"fxsr", "sse", "sse2", "ht", "pni", "pclmulqdq", "vmx", "ssse3", "fma",
	"cx16", "sse4_1", "sse4_2", "x2apic", "movbe", "popcnt", "aes", "xsave",
	"osxsave", "avx", "f16c", "rdrand", "hypervisor", "fsgsbase", "bmi1", "hle",
	"avx2", "smep", "bmi2", "erms", "invpcid", "rtm", "avx512f", "rdseed",
	"adx", "smap", "clflushopt", "clwb", "sha_ni", "syscall", "nx", "pdpe1gb",
	"rdtscp", "lm", "lahf_lm", "svm", "abm", "sse4a", "prefetchw", "topoext",
	"umip", "pku", "ospke", "waitpkg", "gfni", "vaes", "vpclmulqdq", "rdpid",
	"serialize", "ibt",
}

func FeatureName(feature Feature) string {
	if feature >= FeatureCount {
		return ""
	}
	return featureNames[feature]
}
