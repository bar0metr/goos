package console

import "testing"

func captureSerial(state *serialNewlines, output *[]byte, value string) {
	for index := 0; index < len(value); index++ {
		if state.insertCR(value[index]) {
			*output = append(*output, '\r')
		}
		*output = append(*output, value[index])
	}
}

func TestSerialNewlines(t *testing.T) {
	for _, test := range []struct {
		parts []string
		want  string
	}{
		{[]string{"a\nb"}, "a\r\nb"},
		{[]string{"a\r\nb"}, "a\r\nb"},
		{[]string{"a\r", "\n"}, "a\r\n"},
		{[]string{"\n\n"}, "\r\n\r\n"},
		{[]string{"a\r", "", "\n"}, "a\r\n"},
	} {
		var state serialNewlines
		var output []byte
		for _, part := range test.parts {
			captureSerial(&state, &output, part)
		}
		if got := string(output); got != test.want {
			t.Fatalf("parts %q: got %q, want %q", test.parts, got, test.want)
		}
	}
}

func TestSerialStringSharesNewlineState(t *testing.T) {
	var state serialNewlines
	var output []byte
	captureSerial(&state, &output, "a\r")
	captureSerial(&state, &output, "")
	captureSerial(&state, &output, "\n")
	captureSerial(&state, &output, "b\nc")
	if got, want := string(output), "a\r\nb\r\nc"; got != want {
		t.Fatalf("output %q, want %q", got, want)
	}
}
