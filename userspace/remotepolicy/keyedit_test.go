package remotepolicy

import (
	"bytes"
	"strings"
	"testing"
)

func adminLine(t *testing.T, fill byte, comment string) []byte {
	t.Helper()
	return []byte(strings.TrimSuffix(keyLine(t, testPublic(fill)), "\n") + comment)
}
func TestKeyEditorPreservesCommentsAndStableOrder(t *testing.T) {
	first, second := adminLine(t, 1, " first"), adminLine(t, 2, " second\x1b[31m")
	original := append([]byte("# header\r\n\r\n  "), first...)
	original = append(original, []byte("\r\n# middle\n")...)
	added, err := AddAuthorizedKey(original, second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(added[:len(original)], original) {
		t.Fatal("existing bytes rewritten")
	}
	entries, err := ListAuthorizedKeys(added)
	if err != nil || len(entries) != 2 {
		t.Fatal(entries, err)
	}
	fp, _ := Fingerprint(testPublic(1))
	if entries[0].Fingerprint != fp || entries[0].Comment != "first" || entries[1].Comment != "second?[31m" {
		t.Fatal(entries)
	}
	removed, err := RemoveAuthorizedKey(added, []byte(entries[1].Fingerprint))
	if err != nil || !bytes.Equal(removed, original) {
		t.Fatal("remove changed unrelated bytes", err)
	}
	last := adminLine(t, 1, " final")
	removed, err = RemoveAuthorizedKey(last, []byte(fp))
	if err != nil || len(removed) != 0 {
		t.Fatal("unterminated final remove", err)
	}
	if keys, err := ParseAuthorizedKeys(removed); err != nil || keys.Matches(testPublic(1)) {
		t.Fatal("empty was not deny-all")
	}
}
func TestKeyEditorRejectsMalformedDuplicateAndAmbiguous(t *testing.T) {
	line := adminLine(t, 1, " comment")
	fp, _ := Fingerprint(testPublic(1))
	for _, input := range [][]byte{nil, []byte("# comment"), append(append([]byte(nil), line...), '\n'), []byte("ssh-rsa bad"), append(line, append([]byte("\n"), adminLine(t, 2, "")...)...)} {
		if _, err := AddAuthorizedKey(nil, input); err == nil {
			t.Fatalf("bad add accepted %q", input)
		}
	}
	if _, err := AddAuthorizedKey(append(line, '\n'), adminLine(t, 1, " other")); err == nil {
		t.Fatal("duplicate add")
	}
	duplicate := append(append([]byte(nil), line...), '\n')
	duplicate = append(duplicate, line...)
	if _, err := RemoveAuthorizedKey(duplicate, []byte(fp)); err == nil {
		t.Fatal("ambiguous remove")
	}
	unknown, _ := Fingerprint(testPublic(3))
	for _, value := range []string{"", fp[:49], fp + "=", "sha256:" + fp[7:], unknown} {
		if _, err := RemoveAuthorizedKey(line, []byte(value)); err == nil {
			t.Fatal("unknown/noncanonical remove", value)
		}
	}
	malformed := append(append([]byte(nil), line...), []byte("\nmalformed\n")...)
	if _, err := ListAuthorizedKeys(malformed); err == nil {
		t.Fatal("partial list from malformed file")
	}
	if _, err := AddAuthorizedKey(malformed, adminLine(t, 2, "")); err == nil {
		t.Fatal("malformed existing add")
	}
	if _, err := RemoveAuthorizedKey(malformed, []byte(fp)); err == nil {
		t.Fatal("malformed existing remove")
	}
}
func TestKeyEditorBounds(t *testing.T) {
	line := adminLine(t, 1, "")
	maxLine := append(append([]byte(nil), line...), []byte(" "+strings.Repeat("x", MaxAuthorizedKeyLine-len(line)-1))...)
	if _, err := AddAuthorizedKey(nil, maxLine); err != nil {
		t.Fatal("4096 line", err)
	}
	if _, err := AddAuthorizedKey(nil, append(maxLine, 'x')); err == nil {
		t.Fatal("4097 line accepted")
	}
	entries, err := ListAuthorizedKeys(maxLine)
	if err != nil || len(entries[0].Comment) != MaxKeyComment {
		t.Fatal("safe comment bound", err)
	}
	var full []byte
	for i := 1; i <= MaxAuthorizedKeys; i++ {
		full = append(full, adminLine(t, byte(i), "")...)
		full = append(full, '\n')
	}
	if _, err := AddAuthorizedKey(full, adminLine(t, 100, "")); err == nil {
		t.Fatal("65th key")
	}
	var comments []byte
	for len(comments)+4097 <= MaxAuthorizedKeysBytes {
		comments = append(comments, []byte("#"+strings.Repeat("x", 4095)+"\n")...)
	}
	comments = append(comments, []byte("#"+strings.Repeat("x", MaxAuthorizedKeysBytes-len(comments)-1))...)
	if _, err := ParseAuthorizedKeys(comments); err != nil {
		t.Fatal("64KiB fixture", err)
	}
	if _, err := AddAuthorizedKey(comments, line); err == nil {
		t.Fatal("oversized candidate")
	}
	if _, err := ListAuthorizedKeys(append(comments, 'x')); err == nil {
		t.Fatal("oversized list")
	}
}
