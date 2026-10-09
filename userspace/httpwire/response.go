// Package httpwire parses bounded HTTP/1.x response headers and streams bodies.
package httpwire

import (
	"errors"
	"io"
)

const (
	MaxHeaderBytes = 16 << 10
	MaxHeaderLine  = 4096
	MaxHeaders     = 64
	MaxLocation    = 4096
)

var ErrProtocol = errors.New("invalid HTTP response")

type Source interface{ Read([]byte) (int, error) }
type Sink interface{ Write([]byte) error }

type Response struct {
	Header       [MaxHeaderBytes]byte
	Size         int
	Status       int
	Location     [MaxLocation]byte
	LocationSize int
	Length       uint64
	HasLength    bool
	Chunked      bool
}

func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}
func digit(c byte) bool { return c >= '0' && c <= '9' }
func token(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' ||
		c == '!' || c == '#' || c == '$' || c == '%' || c == '&' || c == '\'' || c == '*' || c == '+' || c == '-' || c == '.' || c == '^' || c == '_' || c == '`' || c == '|' || c == '~'
}
func equal(a []byte, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if lower(a[i]) != b[i] {
			return false
		}
	}
	return true
}
func trim(a []byte) []byte {
	for len(a) > 0 && (a[0] == ' ' || a[0] == '\t') {
		a = a[1:]
	}
	for len(a) > 0 && (a[len(a)-1] == ' ' || a[len(a)-1] == '\t') {
		a = a[:len(a)-1]
	}
	return a
}

// Reader retains one TCP read window; no body byte is stored in Response.
type Reader struct {
	Source   Source
	buffer   [4096]byte
	used, at int
}

func (r *Reader) fill() error {
	if r.at < r.used {
		return nil
	}
	n, err := r.Source.Read(r.buffer[:])
	if n < 0 || n > len(r.buffer) {
		return ErrProtocol
	}
	r.at, r.used = 0, n
	if n != 0 {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrProtocol
}
func (r *Reader) byte() (byte, error) {
	if err := r.fill(); err != nil {
		return 0, err
	}
	c := r.buffer[r.at]
	r.at++
	return c, nil
}
func (r *Reader) line(dst []byte) (int, error) {
	n := 0
	for {
		c, err := r.byte()
		if err != nil {
			return 0, err
		}
		if c == '\r' {
			c, err = r.byte()
			if err != nil || c != '\n' {
				return 0, ErrProtocol
			}
			return n, nil
		}
		if c == '\n' || c < ' ' && c != '\t' || c == 127 || n == len(dst) {
			return 0, ErrProtocol
		}
		dst[n] = c
		n++
	}
}

func (r *Reader) Headers(out *Response) error {
	*out = Response{}
	count := 0
	for {
		if out.Size+MaxHeaderLine+2 > len(out.Header) && out.Size >= len(out.Header)-2 {
			return ErrProtocol
		}
		var line [MaxHeaderLine]byte
		n, err := r.line(line[:])
		if err != nil {
			return err
		}
		if out.Size+n+2 > len(out.Header) {
			return ErrProtocol
		}
		copy(out.Header[out.Size:], line[:n])
		out.Size += n
		out.Header[out.Size], out.Header[out.Size+1] = '\r', '\n'
		out.Size += 2
		if count == 0 {
			if n < 12 || !(string(line[:8]) == "HTTP/1.0" || string(line[:8]) == "HTTP/1.1") || line[8] != ' ' || !digit(line[9]) || !digit(line[10]) || !digit(line[11]) || n > 12 && line[12] != ' ' {
				return ErrProtocol
			}
			out.Status = int(line[9]-'0')*100 + int(line[10]-'0')*10 + int(line[11]-'0')
			if out.Status < 100 {
				return ErrProtocol
			}
		} else if n == 0 {
			return nil
		} else {
			if count > MaxHeaders {
				return ErrProtocol
			}
			colon := -1
			for i := 0; i < n; i++ {
				if line[i] == ':' {
					colon = i
					break
				}
				if !token(line[i]) {
					return ErrProtocol
				}
			}
			if colon <= 0 {
				return ErrProtocol
			}
			value := trim(line[colon+1 : n])
			for _, c := range value {
				if c < ' ' && c != '\t' || c == 127 {
					return ErrProtocol
				}
			}
			switch {
			case equal(line[:colon], "content-length"):
				if len(value) == 0 {
					return ErrProtocol
				}
				var length uint64
				for _, c := range value {
					if !digit(c) || length > (^uint64(0)-uint64(c-'0'))/10 {
						return ErrProtocol
					}
					length = length*10 + uint64(c-'0')
				}
				if out.HasLength && out.Length != length {
					return ErrProtocol
				}
				out.Length, out.HasLength = length, true
			case equal(line[:colon], "transfer-encoding"):
				if out.Chunked || !equal(value, "chunked") {
					return ErrProtocol
				}
				out.Chunked = true
			case equal(line[:colon], "location"):
				if len(value) == 0 || len(value) > MaxLocation || out.LocationSize != 0 {
					return ErrProtocol
				}
				copy(out.Location[:], value)
				out.LocationSize = len(value)
			}
		}
		count++
		if out.Chunked && out.HasLength {
			return ErrProtocol
		}
	}
}

func (r *Reader) take(n uint64, sink Sink) error {
	for n > 0 {
		if err := r.fill(); err != nil {
			return ErrProtocol
		}
		part := r.used - r.at
		if uint64(part) > n {
			part = int(n)
		}
		if err := sink.Write(r.buffer[r.at : r.at+part]); err != nil {
			return err
		}
		r.at += part
		n -= uint64(part)
	}
	return nil
}
func (r *Reader) Body(response *Response, head bool, sink Sink) error {
	if head || response.Status < 200 || response.Status == 204 || response.Status == 304 {
		return nil
	}
	if response.Chunked {
		return r.chunks(sink)
	}
	if response.HasLength {
		return r.take(response.Length, sink)
	}
	for {
		if r.at < r.used {
			if err := sink.Write(r.buffer[r.at:r.used]); err != nil {
				return err
			}
			r.at = r.used
		}
		n, err := r.Source.Read(r.buffer[:])
		if n < 0 || n > len(r.buffer) {
			return ErrProtocol
		}
		if n > 0 {
			if e := sink.Write(r.buffer[:n]); e != nil {
				return e
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if n == 0 {
			return ErrProtocol
		}
	}
}
func (r *Reader) chunks(sink Sink) error {
	var line [MaxHeaderLine]byte
	for {
		n, err := r.line(line[:])
		if err != nil {
			return ErrProtocol
		}
		var size uint64
		digits := 0
		for i := 0; i < n && line[i] != ';'; i++ {
			c := lower(line[i])
			var v uint64
			switch {
			case digit(c):
				v = uint64(c - '0')
			case c >= 'a' && c <= 'f':
				v = uint64(c - 'a' + 10)
			default:
				return ErrProtocol
			}
			if size > (^uint64(0)-v)/16 {
				return ErrProtocol
			}
			size = size*16 + v
			digits++
		}
		if digits == 0 {
			return ErrProtocol
		}
		if size == 0 {
			count, total := 0, 0
			for {
				n, err := r.line(line[:])
				if err != nil {
					return ErrProtocol
				}
				total += n + 2
				if total > MaxHeaderBytes || count > MaxHeaders {
					return ErrProtocol
				}
				if n == 0 {
					return nil
				}
				colon := 0
				for colon < n && token(line[colon]) {
					colon++
				}
				if colon == 0 || colon == n || line[colon] != ':' {
					return ErrProtocol
				}
				count++
			}
		}
		if err := r.take(size, sink); err != nil {
			return err
		}
		a, err := r.byte()
		if err != nil || a != '\r' {
			return ErrProtocol
		}
		b, err := r.byte()
		if err != nil || b != '\n' {
			return ErrProtocol
		}
	}
}
