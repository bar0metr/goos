package httpwire

import (
	"bytes"
	"io"
	"testing"
)

type source struct {
	data []byte
	step int
}

func (s *source) Read(p []byte) (int, error) {
	if len(s.data) == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > s.step {
		n = s.step
	}
	if n > len(s.data) {
		n = len(s.data)
	}
	copy(p, s.data[:n])
	s.data = s.data[n:]
	return n, nil
}

type sink struct{ bytes.Buffer }

func (s *sink) Write(p []byte) error { _, err := s.Buffer.Write(p); return err }

func TestFraming(t *testing.T) {
	for _, tc := range []struct {
		wire string
		head bool
		body string
		bad  bool
	}{
		{"HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello", false, "hello", false},
		{"HTTP/1.0 200 OK\r\n\r\nhello", false, "hello", false},
		{"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5;x=y\r\nhello\r\n0\r\nX-End: yes\r\n\r\n", false, "hello", false},
		{"HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhi", false, "", true},
		{"HTTP/1.1 204 No Content\r\nContent-Length: 5\r\n\r\nhello", false, "", false},
		{"HTTP/1.1 304 Not Modified\r\n\r\nhello", false, "", false},
		{"HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello", true, "", false},
		{"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nContent-Length: 3\r\n\r\nhi", false, "", true},
		{"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n1\r\naX\r\n0\r\n\r\n", false, "", true},
		{"HTTP/1.1 200 OK\r\nTransfer-Encoding: gzip, chunked\r\n\r\n", false, "", true},
		{"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nContent-Length: 5\r\n\r\n", false, "", true},
		{"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nZ\r\n", false, "", true},
	} {
		for _, step := range []int{1, 4096} {
			r := Reader{Source: &source{data: []byte(tc.wire), step: step}}
			var response Response
			out := &sink{}
			err := r.Headers(&response)
			if err == nil {
				err = r.Body(&response, tc.head, out)
			}
			if (err != nil) != tc.bad || !tc.bad && out.String() != tc.body {
				t.Fatalf("step %d wire %q: body %q error %v", step, tc.wire, out.String(), err)
			}
		}
	}
}
