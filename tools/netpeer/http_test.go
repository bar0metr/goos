package main

import (
	"bytes"
	"testing"
)

func TestHTTPPeer(t *testing.T) {
	for _, tc := range []struct{ request, expected string }{
		{"GET /hello HTTP/1.1\r\nHost: http.test\r\n\r\n", "\r\n\r\nhello"},
		{"HEAD /hello HTTP/1.1\r\nHost: http.test\r\n\r\n", "Content-Length: 5\r\n"},
		{"POST /post HTTP/1.1\r\nContent-Length: 3\r\n\r\na=b", "\r\n\r\na=b"},
		{"GET /chunked HTTP/1.1\r\n\r\n", "5;test=1\r\nhello\r\n0\r\n"},
		{"GET /redirect HTTP/1.1\r\n\r\n", "Location: /hello\r\n"},
	} {
		response, _, complete := httpReply([]byte(tc.request))
		if !complete || !bytes.Contains(response, []byte(tc.expected)) {
			t.Fatalf("request %q: response %q", tc.request, response)
		}
	}
	if _, _, complete := httpReply([]byte("POST /post HTTP/1.1\r\nContent-Length: 3\r\n\r\na")); complete {
		t.Fatal("accepted partial body")
	}
}
