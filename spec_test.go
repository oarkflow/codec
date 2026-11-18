package codec

import (
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// The byte examples in README.md section 10.6 and the fingerprints in 10.2 are
// produced here, so the specification cannot drift away from the encoder.
func TestSpecWorkedExamples(t *testing.T) {
	type S struct {
		A uint32
		B string
	}
	type P struct {
		X int32
		Y []byte
		Z []uint16
	}
	type Pt struct {
		TS   int64
		Host string
	}
	var anyHi any = "hi"
	pts := []Pt{{1000, "web"}, {1001, "web"}, {1002, "web"}, {1003, "web"}}

	cases := []struct {
		name      string
		got       func() ([]byte, error)
		want      string
		inReadme  []string // fragments that must appear in the README
		fpSchema  string
		fpWant    string
		evolvable bool
	}{
		{"struct", func() ([]byte, error) { return Marshal(&S{300, "hi"}) },
			"c0582db81d03ac02046869", []string{"c0 | 58 2d b8 1d | 03 | ac 02 | 04 | 68 69"}, "{A:3.4;B:6.16;}", "582db81d", false},
		{"struct evolvable", func() ([]byte, error) { return MarshalEvolvable(&S{300, "hi"}) },
			"c4582db81d010203ac02046869", []string{"c4 | 58 2d b8 1d | 01 02 | 03 ac 02 04 68 69"}, "", "", true},
		{"several kinds", func() ([]byte, error) { return Marshal(&P{-2, []byte{1, 2}, []uint16{1, 2, 4}}) },
			"c04c11b8b607030406010203020204", []string{"c0 | 4c 11 b8 b6 | 07 | 03 | 04 06 | 01 02 | 03 02 02 04"}, "{X:2.4;Y:[]3.1;Z:[]3.2;}", "4c11b8b6", false},
		{"interface root", func() ([]byte, error) { return Marshal(&anyHi) },
			"c072e630f8010206026869", []string{"c0 | 72 e6 30 f8 | 01 | 02 | 06 02 68 69", "`12.16W`"}, "12.16W", "72e630f8", false},
		{"map", func() ([]byte, error) { return Marshal(map[string]int64{"a": 1, "b": 2}) },
			"c02104d5e30102050403046162020202", []string{"c0 | 21 04 d5 e3 | 01 | 02 | 05 | 04 03 04 61 62 | 02 02 02", "`map[6.16]2.8W`"}, "map[6.16]2.8W", "2104d5e3", false},
		{"columnar", func() ([]byte, error) { return Marshal(&pts) },
			"c0bc42534a010209030801d00f02e40003776562037765620377656203776562",
			[]string{"c0 | bc 42 53 4a | 01 | 02 | 09 | 03 08 | 01 d0 0f 02 e4 | 00 03 77 65 62 03 77 65 62 03 77 65 62 03 77 65 62", "`[]{TS:2.8;Host:6.16;}W`"},
			"[]{TS:2.8;Host:6.16;}W", "bc42534a", false},
	}
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Skip("README.md not found")
	}
	for _, c := range cases {
		b, err := c.got()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := hex.EncodeToString(b); got != c.want {
			t.Errorf("%s: encoder produced %s, README documents %s", c.name, got, c.want)
		}
		for _, frag := range c.inReadme {
			if !strings.Contains(string(readme), frag) {
				t.Errorf("%s: README does not contain %q", c.name, frag)
			}
		}
		if c.fpSchema != "" {
			// recompute the documented fingerprint from the documented schema string
			if got := specFingerprint(c.fpSchema); got != c.fpWant {
				t.Errorf("%s: fingerprint of %q is %s, README implies %s", c.name, c.fpSchema, got, c.fpWant)
			}
			if !strings.HasPrefix(hex.EncodeToString(b)[2:], c.fpWant) {
				t.Errorf("%s: message does not start with fingerprint %s", c.name, c.fpWant)
			}
		}
	}
}

// specFingerprint implements the README's fingerprint definition independently
// of the encoder: FNV-1a 64 of the schema string, folded to 32 bits, little-endian.
func specFingerprint(schema string) string {
	h := uint64(14695981039346656037)
	for i := 0; i < len(schema); i++ {
		h ^= uint64(schema[i])
		h *= 1099511628211
	}
	f := uint32(h) ^ uint32(h>>32)
	return hex.EncodeToString([]byte{byte(f), byte(f >> 8), byte(f >> 16), byte(f >> 24)})
}
