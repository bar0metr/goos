// Package fetch implements bounded HTTP requests and redirect policy.
package fetch

import (
	"errors"
	"goos/userspace/hostname"
	"goos/userspace/httpwire"
	"goos/userspace/network"
)

const MaxURL = 4096
const MaxRequestHeaders = 16
const MaxRedirects = 5

var ErrUsage = errors.New("invalid fetch arguments")
var ErrURL = errors.New("invalid HTTP URL")
var ErrRedirect = errors.New("invalid HTTP redirect")
var ErrRequest = errors.New("HTTP request too large")

type URL struct {
	Host       [hostname.MaxText]byte
	HostSize   int
	Port       uint16
	Target     [MaxURL]byte
	TargetSize int
	Numeric    bool
	Address    network.IPv4
}

func (u *URL) HostBytes() []byte   { return u.Host[:u.HostSize] }
func (u *URL) TargetBytes() []byte { return u.Target[:u.TargetSize] }

func ParseURL(raw []byte, out *URL) error {
	*out = URL{}
	if len(raw) < 8 || len(raw) > MaxURL || string(raw[:7]) != "http://" {
		return ErrURL
	}
	for _, c := range raw {
		if c < ' ' || c == 127 {
			return ErrURL
		}
	}
	part := raw[7:]
	authorityEnd := 0
	for authorityEnd < len(part) && part[authorityEnd] != '/' && part[authorityEnd] != '?' && part[authorityEnd] != '#' {
		authorityEnd++
	}
	authority := part[:authorityEnd]
	if len(authority) == 0 {
		return ErrURL
	}
	colon := len(authority)
	for i, c := range authority {
		if c == '@' || c == '[' || c == ']' {
			return ErrURL
		}
		if c == ':' {
			if colon != len(authority) {
				return ErrURL
			}
			colon = i
		}
	}
	host := authority[:colon]
	if len(host) == 0 || len(host) > len(out.Host) {
		return ErrURL
	}
	if ip, ok := network.ParseIPv4(host); ok {
		out.Numeric, out.Address = true, ip
		copy(out.Host[:], host)
		out.HostSize = len(host)
	} else {
		for _, c := range host {
			if c >= '0' && c <= '9' || c == '.' {
				continue
			}
			goto name
		}
		return ErrURL
	name:
		name, ok := hostname.Parse(host)
		if !ok {
			return ErrURL
		}
		copy(out.Host[:], name.Bytes())
		out.HostSize = int(name.Size)
	}
	out.Port = 80
	if colon != len(authority) {
		port := authority[colon+1:]
		if len(port) == 0 || len(port) > 5 {
			return ErrURL
		}
		var value uint32
		for _, c := range port {
			if c < '0' || c > '9' {
				return ErrURL
			}
			value = value*10 + uint32(c-'0')
		}
		if value == 0 || value > 65535 {
			return ErrURL
		}
		out.Port = uint16(value)
	}
	target := part[authorityEnd:]
	for i, c := range target {
		if c == '#' {
			target = target[:i]
			break
		}
	}
	if len(target) == 0 {
		out.Target[0], out.TargetSize = '/', 1
		return nil
	}
	if target[0] == '?' {
		out.Target[0] = '/'
		out.TargetSize = 1
	}
	if target[0] != '/' && target[0] != '?' {
		return ErrURL
	}
	if len(target)+out.TargetSize > len(out.Target) {
		return ErrURL
	}
	out.TargetSize += copy(out.Target[out.TargetSize:], target)
	return nil
}

type Header struct {
	Name      [64]byte
	NameSize  int
	Value     [4096]byte
	ValueSize int
}

func (h *Header) NameBytes() []byte  { return h.Name[:h.NameSize] }
func (h *Header) ValueBytes() []byte { return h.Value[:h.ValueSize] }
func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}
func equals(a []byte, b string) bool {
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
func token(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '-' || c == '_' || c == '!' || c == '#' || c == '$' || c == '%' || c == '&' || c == '\'' || c == '*' || c == '+' || c == '.' || c == '^' || c == '`' || c == '|' || c == '~'
}
func ParseHeader(raw []byte, out *Header) error {
	*out = Header{}
	colon := 0
	for colon < len(raw) && raw[colon] != ':' {
		colon++
	}
	if colon == 0 || colon >= len(raw) || colon > len(out.Name) {
		return ErrUsage
	}
	for _, c := range raw[:colon] {
		if !token(c) {
			return ErrUsage
		}
	}
	if equals(raw[:colon], "content-length") || equals(raw[:colon], "transfer-encoding") || equals(raw[:colon], "connection") {
		return ErrUsage
	}
	value := raw[colon+1:]
	for len(value) > 0 && (value[0] == ' ' || value[0] == '\t') {
		value = value[1:]
	}
	for len(value) > 0 && (value[len(value)-1] == ' ' || value[len(value)-1] == '\t') {
		value = value[:len(value)-1]
	}
	if len(value) > len(out.Value) {
		return ErrUsage
	}
	for _, c := range value {
		if c < ' ' && c != '\t' || c == 127 {
			return ErrUsage
		}
	}
	out.NameSize = copy(out.Name[:], raw[:colon])
	out.ValueSize = copy(out.Value[:], value)
	return nil
}

type Options struct {
	Head, Include, Verbose, Follow, RemoteName bool
	Post                                       bool
	Data                                       []byte
	Output                                     []byte
	Headers                                    [MaxRequestHeaders]Header
	HeaderCount                                int
	URL                                        []byte
}

func Parse(args [][]byte, out *Options) error {
	*out = Options{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) == 0 {
			return ErrUsage
		}
		if a[0] != '-' {
			if out.URL != nil {
				return ErrUsage
			}
			out.URL = a
			continue
		}
		switch string(a) {
		case "-I":
			out.Head = true
		case "-i":
			out.Include = true
		case "-v":
			out.Verbose = true
		case "-L":
			out.Follow = true
		case "-O":
			out.RemoteName = true
		case "-d", "-H", "-o":
			i++
			if i == len(args) {
				return ErrUsage
			}
			switch string(a) {
			case "-d":
				if out.Post {
					return ErrUsage
				}
				out.Post, out.Data = true, args[i]
			case "-o":
				if out.Output != nil {
					return ErrUsage
				}
				out.Output = args[i]
			case "-H":
				if out.HeaderCount == MaxRequestHeaders || ParseHeader(args[i], &out.Headers[out.HeaderCount]) != nil {
					return ErrUsage
				}
				out.HeaderCount++
			}
		default:
			return ErrUsage
		}
	}
	if out.URL == nil || out.Head && out.Post || out.RemoteName && out.Output != nil {
		return ErrUsage
	}
	if out.Output != nil && len(out.Output) == 0 {
		return ErrUsage
	}
	var parsed URL
	if ParseURL(out.URL, &parsed) != nil {
		return ErrUsage
	}
	return nil
}

func decimal(dst []byte, n uint64) []byte {
	if n >= 10 {
		dst = decimal(dst, n/10)
	}
	return append(dst, byte('0'+n%10))
}
func custom(o *Options, name string) bool {
	for i := 0; i < o.HeaderCount; i++ {
		if equals(o.Headers[i].NameBytes(), name) {
			return true
		}
	}
	return false
}

func EncodeRequest(dst []byte, u *URL, o *Options, method string) ([]byte, error) {
	dst = dst[:0]
	put := func(p []byte) bool {
		if len(dst)+len(p) > cap(dst) {
			return false
		}
		dst = append(dst, p...)
		return true
	}
	if !put([]byte(method)) || !put([]byte(" ")) || !put(u.TargetBytes()) || !put([]byte(" HTTP/1.1\r\n")) {
		return nil, ErrRequest
	}
	if !custom(o, "host") {
		var host [hostname.MaxText + 6]byte
		value := append(host[:0], u.HostBytes()...)
		if u.Port != 80 {
			value = append(value, ':')
			value = decimal(value, uint64(u.Port))
		}
		if !put([]byte("Host: ")) || !put(value) || !put([]byte("\r\n")) {
			return nil, ErrRequest
		}
	}
	if !custom(o, "user-agent") && !put([]byte("User-Agent: goos-fetch/1\r\n")) {
		return nil, ErrRequest
	}
	if !custom(o, "accept") && !put([]byte("Accept: */*\r\n")) {
		return nil, ErrRequest
	}
	if method == "POST" {
		if !custom(o, "content-type") && !put([]byte("Content-Type: application/x-www-form-urlencoded\r\n")) {
			return nil, ErrRequest
		}
		var digits [20]byte
		if !put([]byte("Content-Length: ")) || !put(decimal(digits[:0], uint64(len(o.Data)))) || !put([]byte("\r\n")) {
			return nil, ErrRequest
		}
	}
	if !put([]byte("Connection: close\r\n")) {
		return nil, ErrRequest
	}
	for i := 0; i < o.HeaderCount; i++ {
		h := &o.Headers[i]
		if !put(h.NameBytes()) || !put([]byte(": ")) || !put(h.ValueBytes()) || !put([]byte("\r\n")) {
			return nil, ErrRequest
		}
	}
	if !put([]byte("\r\n")) {
		return nil, ErrRequest
	}
	return dst, nil
}

type Connection interface {
	httpwire.Source
	Write([]byte) (int, error)
	Close()
}
type Network interface {
	Resolve([]byte, *[8]network.IPv4) (int, error)
	Connect(network.IPv4, uint16) (Connection, error)
}
type Output interface {
	httpwire.Sink
	Close() error
}
type Environment interface {
	Network
	Open([]byte) (Output, error)
	Stdout() Output
	Diagnostic([]byte)
}

func diagnostic(env Environment, prefix byte, data []byte) {
	var line [MaxURL + 8]byte
	size := 0
	for _, c := range data {
		if c == '\r' {
			continue
		}
		if c == '\n' {
			if size != 0 {
				var output [MaxURL + 12]byte
				output[0], output[1] = prefix, ' '
				copy(output[2:], line[:size])
				output[size+2] = '\n'
				env.Diagnostic(output[:size+3])
				size = 0
			}
			continue
		}
		if size == len(line) {
			break
		}
		if c < ' ' || c > '~' {
			c = '?'
		}
		line[size] = c
		size++
	}
	if size != 0 {
		var output [MaxURL + 12]byte
		output[0], output[1] = prefix, ' '
		copy(output[2:], line[:size])
		output[size+2] = '\n'
		env.Diagnostic(output[:size+3])
	}
}

func filename(u *URL) []byte {
	path := u.TargetBytes()
	for i, c := range path {
		if c == '?' {
			path = path[:i]
			break
		}
	}
	start := 0
	for i, c := range path {
		if c == '/' {
			start = i + 1
		}
	}
	name := path[start:]
	if len(name) == 0 || len(name) == 1 && name[0] == '.' || len(name) == 2 && name[0] == '.' && name[1] == '.' {
		return nil
	}
	return name
}
func redirect(status int) bool {
	return status == 301 || status == 302 || status == 303 || status == 307 || status == 308
}
func nextURL(base *URL, location []byte, out *URL) error {
	if len(location) == 0 || len(location) > MaxURL {
		return ErrRedirect
	}
	if location[0] == '/' && (len(location) == 1 || location[1] != '/') {
		*out = *base
		out.TargetSize = 0
		for i, c := range location {
			if c == '#' {
				location = location[:i]
				break
			}
		}
		if len(location) == 0 || len(location) > len(out.Target) {
			return ErrRedirect
		}
		out.TargetSize = copy(out.Target[:], location)
		return nil
	}
	if ParseURL(location, out) != nil {
		return ErrRedirect
	}
	return nil
}

// Run executes one request per hop and emits only the final response.
func Run(o *Options, env Environment) error {
	var current URL
	if err := ParseURL(o.URL, &current); err != nil {
		return err
	}
	method := "GET"
	if o.Head {
		method = "HEAD"
	}
	if o.Post {
		method = "POST"
	}
	for hop := 0; ; hop++ {
		var addresses [8]network.IPv4
		count := 1
		if current.Numeric {
			addresses[0] = current.Address
		} else {
			if o.Verbose {
				var line [300]byte
				value := append(line[:0], "resolving "...)
				value = append(value, current.HostBytes()...)
				diagnostic(env, '*', value)
			}
			var err error
			count, err = env.Resolve(current.HostBytes(), &addresses)
			if err != nil || count < 1 || count > len(addresses) {
				if err != nil {
					return err
				}
				return ErrURL
			}
		}
		var conn Connection
		var err error
		for i := 0; i < count; i++ {
			if o.Verbose {
				var line [300]byte
				value := append(line[:0], "connecting to "...)
				value = network.AppendIPv4(value, addresses[i])
				value = append(value, ':')
				value = decimal(value, uint64(current.Port))
				diagnostic(env, '*', value)
			}
			conn, err = env.Connect(addresses[i], current.Port)
			if err == nil {
				break
			}
		}
		if err != nil {
			return err
		}
		var request [16 << 10]byte
		encoded, err := EncodeRequest(request[:0], &current, o, method)
		if err != nil {
			conn.Close()
			return err
		}
		if o.Verbose {
			diagnostic(env, '>', encoded)
		}
		for len(encoded) > 0 {
			var n int
			n, err = conn.Write(encoded)
			if err != nil || n <= 0 || n > len(encoded) {
				conn.Close()
				if err != nil {
					return err
				}
				return ErrRequest
			}
			encoded = encoded[n:]
		}
		if method == "POST" {
			body := o.Data
			for len(body) > 0 {
				var n int
				n, err = conn.Write(body)
				if err != nil || n <= 0 || n > len(body) {
					conn.Close()
					if err != nil {
						return err
					}
					return ErrRequest
				}
				body = body[n:]
			}
		}
		reader := httpwire.Reader{Source: conn}
		var response httpwire.Response
		for {
			err = reader.Headers(&response)
			if err != nil || response.Status >= 200 {
				break
			}
		}
		if err != nil {
			conn.Close()
			return err
		}
		if o.Verbose {
			diagnostic(env, '<', response.Header[:response.Size])
		}
		if o.Follow && redirect(response.Status) {
			if hop == MaxRedirects {
				conn.Close()
				return ErrRedirect
			}
			var next URL
			err = nextURL(&current, response.Location[:response.LocationSize], &next)
			conn.Close()
			if err != nil {
				return err
			}
			current = next
			if response.Status == 303 || (response.Status == 301 || response.Status == 302) && method == "POST" {
				method = "GET"
			}
			continue
		}
		out := env.Stdout()
		own := false
		if o.Output != nil || o.RemoteName {
			path := o.Output
			if o.RemoteName {
				path = filename(&current)
				if path == nil {
					conn.Close()
					return ErrURL
				}
			}
			out, err = env.Open(path)
			if err != nil {
				conn.Close()
				return err
			}
			own = true
		}
		if o.Include || o.Head {
			err = out.Write(response.Header[:response.Size])
		}
		if err == nil {
			err = reader.Body(&response, o.Head, out)
		}
		conn.Close()
		if own {
			closeErr := out.Close()
			if err == nil {
				err = closeErr
			}
		}
		return err
	}
}
