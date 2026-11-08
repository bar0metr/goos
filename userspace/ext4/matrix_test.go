package ext4

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const matrixDoc = "../../docs/development/ext4-feature-matrix.md"

// renderMatrix renders the feature table the way the document carries it.
func renderMatrix() string {
	var text strings.Builder
	text.WriteString("| Feature | Word | Bit | Support | Constraint |\n")
	text.WriteString("|---|---|---|---|---|\n")
	for _, feature := range Features {
		text.WriteString("| `" + feature.Name + "` | " + feature.Class.String() + " | `" +
			hex32(feature.Mask) + "` | " + feature.Support.String() + " | " + feature.Note + " |\n")
	}
	return text.String()
}

// TestFeatureMatrixDocument keeps the document's table equal to Features.
// GOOS_UPDATE_MATRIX=1 rewrites the table instead.
func TestFeatureMatrixDocument(t *testing.T) {
	const begin, end = "<!-- matrix:begin -->\n", "<!-- matrix:end -->"
	path := filepath.FromSlash(matrixDoc)
	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(text)
	start, stop := strings.Index(doc, begin), strings.Index(doc, end)
	if start < 0 || stop < start {
		t.Fatalf("%s lacks the matrix markers", matrixDoc)
	}
	want := renderMatrix()
	if doc[start+len(begin):stop] == want {
		return
	}
	if os.Getenv("GOOS_UPDATE_MATRIX") != "" {
		doc = doc[:start+len(begin)] + want + doc[stop:]
		if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("%s differs from Features; rerun with GOOS_UPDATE_MATRIX=1", matrixDoc)
}

func TestFeatureMatrixUnique(t *testing.T) {
	seen := map[string]bool{}
	bits := map[FeatureClass]uint32{}
	for _, feature := range Features {
		if seen[feature.Name] || bits[feature.Class]&feature.Mask != 0 || feature.Mask&(feature.Mask-1) != 0 {
			t.Fatalf("%s duplicates a name or bit, or spans several bits", feature.Name)
		}
		seen[feature.Name] = true
		bits[feature.Class] |= feature.Mask
	}
}

func TestNegotiateOrder(t *testing.T) {
	canonical := FeatureSet{
		Compat:   compatHasJournal | compatExtAttr | compatResizeInode | compatDirIndex,
		Incompat: incompatFiletype | incompatExtents | incompat64Bit | incompatFlexBG,
		ROCompat: roCompatSparseSuper | roCompatLargeFile | roCompatHugeFile | roCompatDirNlink | roCompatExtraIsize | roCompatMetadataCsum,
	}
	if got := Negotiate(canonical); got.Support != ReadWrite {
		t.Fatalf("canonical: %+v", got)
	}
	withoutJournal := canonical
	withoutJournal.Compat &^= compatHasJournal
	if got := Negotiate(withoutJournal); got.Support != ReadOnly || !strings.Contains(got.Reason, "has_journal") {
		t.Fatalf("no journal: %+v", got)
	}
	unknownCompat := canonical
	unknownCompat.Compat |= 0x80000000
	if got := Negotiate(unknownCompat); got.Support != ReadOnly {
		t.Fatalf("unknown compat: %+v", got)
	}
	mixed := canonical
	mixed.ROCompat |= roCompatQuota
	mixed.Incompat |= incompatEncrypt
	if got := Negotiate(mixed); got.Support != Reject || !strings.Contains(got.Reason, "encrypt") {
		t.Fatalf("encrypt and quota: %+v", got)
	}
	recovering := canonical
	recovering.Incompat |= incompatRecover
	if got := Negotiate(recovering); got.Support != ReadWrite {
		t.Fatalf("needs_recovery is a state, not a feature: %+v", got)
	}
}
