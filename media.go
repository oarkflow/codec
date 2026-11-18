package codec

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// Wire formats and media types, for carrying Strata over any transport that
// labels its payloads (HTTP, message queues, files). Nothing here depends on a
// transport package: they are plain functions over strings and bytes.

// Format is one of the three representations of a Strata message.
type Format int

const (
	FormatBinary Format = iota // the compact binary protocol
	FormatText                 // the readable text notation (MarshalText)
	FormatBase64               // the binary protocol armored as base64 text
)

// Media types for the formats.
const (
	MediaBinary = "application/x-strata"
	MediaText   = "text/x-strata"
	MediaBase64 = "application/x-strata+base64"
)

func (f Format) String() string {
	switch f {
	case FormatText:
		return "text"
	case FormatBase64:
		return "base64"
	}
	return "binary"
}

// MediaType is the media type for f (with a charset for text formats).
func (f Format) MediaType() string {
	switch f {
	case FormatText:
		return MediaText + "; charset=utf-8"
	case FormatBase64:
		return MediaBase64
	}
	return MediaBinary
}

// ParseFormat understands "binary", "text" and "base64" (and a few aliases).
func ParseFormat(name string) (Format, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "binary", "bin", "strata":
		return FormatBinary, true
	case "text", "txt":
		return FormatText, true
	case "base64", "b64", "armor":
		return FormatBase64, true
	}
	return FormatBinary, false
}

func splitMedia(s string) (mt string, q float64) {
	q = 1
	parts := strings.Split(s, ";")
	mt = strings.ToLower(strings.TrimSpace(parts[0]))
	for _, p := range parts[1:] {
		k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
		if strings.EqualFold(k, "q") {
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				q = f
			}
		}
	}
	return
}

// FormatForMedia maps a media type (parameters such as charset are ignored)
// to a Format.
func FormatForMedia(mediaType string) (Format, bool) {
	switch mt, _ := splitMedia(mediaType); mt {
	case MediaBinary, "application/strata":
		return FormatBinary, true
	case MediaText, "text/strata":
		return FormatText, true
	case MediaBase64:
		return FormatBase64, true
	}
	return FormatBinary, false
}

// Negotiate picks a Format from an HTTP-style Accept header value, honoring
// q-values. text/plain and */* are accepted too: text/plain selects the text
// notation, and */* (or an empty header) selects fallback.
func Negotiate(accept string, fallback Format) Format {
	best, bestQ := fallback, -1.0
	for _, part := range strings.Split(accept, ",") {
		mt, q := splitMedia(part)
		if mt == "" || q <= 0 {
			continue
		}
		f, ok := FormatForMedia(mt)
		switch {
		case mt == "text/plain":
			f, ok = FormatText, true
		case mt == "*/*" || mt == "text/*" || mt == "application/*":
			f, ok = fallback, true
		}
		if ok && q > bestQ {
			best, bestQ = f, q
		}
	}
	return best
}

// Detect decides how a payload labeled with contentType is encoded. Strata
// media types are exact. Generic labels that tools attach by default (empty,
// text/plain, application/octet-stream, curl's
// application/x-www-form-urlencoded) are sniffed: a payload made only of
// base64 characters, with no spaces or newlines inside, is base64 (if it turns
// out not to decode, DecodeAs tries it as text); anything else is text.
// application/octet-stream defaults to binary. Any other label is an error.
func Detect(contentType string, body []byte) (Format, error) {
	if f, ok := FormatForMedia(contentType); ok {
		return f, nil
	}
	mt, _ := splitMedia(contentType)
	switch mt {
	case "", "text/plain", "application/octet-stream", "application/x-www-form-urlencoded":
		if mt == "application/octet-stream" {
			return FormatBinary, nil
		}
		if t := bytes.TrimSpace(body); len(t) > 0 && isBase64Text(t) {
			return FormatBase64, nil
		}
		return FormatText, nil
	}
	return FormatBinary, fmt.Errorf("codec: unsupported content type %q (use %s, %s or %s)", contentType, MediaBinary, MediaText, MediaBase64)
}

func isBase64Text(b []byte) bool {
	for _, c := range b {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '+', c == '/', c == '=':
		default:
			return false
		}
	}
	return true
}

// EncodeAs appends v to dst in format f.
func EncodeAs(dst []byte, f Format, v any) ([]byte, error) {
	switch f {
	case FormatText:
		return AppendText(dst, v)
	case FormatBase64:
		return AppendBase64(dst, v)
	}
	return Append(dst, v)
}

// DecodeAs decodes body, labeled with contentType, into v (a pointer), and
// reports which format it was in. See Detect for how generic labels are handled.
func DecodeAs(contentType string, body []byte, v any) (Format, error) {
	f, err := Detect(contentType, body)
	if err != nil {
		return f, err
	}
	switch f {
	case FormatText:
		err = UnmarshalText(body, v)
	case FormatBase64:
		err = UnmarshalBase64(body, v)
		if _, exact := FormatForMedia(contentType); err != nil && !exact {
			// sniffing guessed base64, but a bare number like 42 looks the same
			if err2 := UnmarshalText(body, v); err2 == nil {
				return FormatText, nil
			}
		}
	default:
		err = Unmarshal(body, v)
	}
	return f, err
}
