package codec

import (
	"bytes"
	"encoding/base64"
	"sync"
)

// Base64 armor: the binary message as URL-safe base64 text (no padding).
// Lossless and about 4/3 the binary size. Use it where a payload must be one
// opaque token: a URL parameter, a header, an environment variable. For text a
// person can read and write, use MarshalText (textfmt.go).

var b64 = base64.RawURLEncoding

var textPool = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}

// MarshalBase64 encodes v and returns the message as URL-safe base64 text.
func MarshalBase64(v any) ([]byte, error) { return AppendBase64(nil, v) }

// AppendBase64 is MarshalBase64 appending to dst.
func AppendBase64(dst []byte, v any) ([]byte, error) {
	bp := textPool.Get().(*[]byte)
	defer textPool.Put(bp)
	bin, err := Append((*bp)[:0], v)
	if cap(bin) <= 1<<20 {
		*bp = bin[:0]
	}
	if err != nil {
		return dst, err
	}
	return b64.AppendEncode(dst, bin), nil
}

// UnmarshalBase64 decodes text produced by MarshalBase64 into v (a pointer).
// Surrounding whitespace is ignored, and standard base64 (+ / and = padding)
// is accepted as well as the URL-safe form.
func UnmarshalBase64(text []byte, v any) error {
	text = bytes.TrimSpace(text)
	if bytes.ContainsAny(text, "+/=") {
		norm := make([]byte, 0, len(text))
		for _, c := range text {
			switch c {
			case '+':
				c = '-'
			case '/':
				c = '_'
			case '=':
				continue
			}
			norm = append(norm, c)
		}
		text = norm
	}
	bp := textPool.Get().(*[]byte)
	defer textPool.Put(bp)
	bin := (*bp)[:0]
	if n := b64.DecodedLen(len(text)); cap(bin) < n {
		bin = make([]byte, 0, n)
	}
	bin, err := b64.AppendDecode(bin, text)
	if cap(bin) <= 1<<20 {
		*bp = bin[:0]
	}
	if err != nil {
		return err
	}
	return Unmarshal(bin, v)
}
