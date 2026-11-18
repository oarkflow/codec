package store

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

func TestTreeAgainstMap(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	tr := &tree{}
	ref := map[string]loc{}
	rk := func() string {
		n := r.Intn(6)
		b := make([]byte, n)
		for i := range b {
			b[i] = "abc/"[r.Intn(4)]
		}
		return string(b)
	}
	for i := 0; i < 20000; i++ {
		k := rk()
		switch r.Intn(3) {
		case 0, 1:
			l := loc{uint32(i), uint32(i), uint32(i)}
			old, had := tr.set([]byte(k), l)
			o2, h2 := ref[k]
			if had != h2 || old != o2 {
				t.Fatalf("set %q: %v %v vs %v %v", k, old, had, o2, h2)
			}
			ref[k] = l
		case 2:
			old, had := tr.del([]byte(k))
			o2, h2 := ref[k]
			if had != h2 || old != o2 {
				t.Fatalf("del %q", k)
			}
			delete(ref, k)
		}
		if tr.n != len(ref) {
			t.Fatalf("count %d vs %d", tr.n, len(ref))
		}
		if i%500 == 0 {
			var want []string
			prefix := rk()
			after := rk()
			for k := range ref {
				if strings.HasPrefix(k, prefix) && k > after {
					want = append(want, k)
				}
			}
			sort.Strings(want)
			var got []string
			tr.scan([]byte(prefix), []byte(after), true, func(k []byte, l loc) bool {
				got = append(got, string(k))
				return true
			})
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("scan prefix=%q after=%q\n got %q\nwant %q", prefix, after, got, want)
			}
		}
	}
	for k, l := range ref {
		if g, ok := tr.get([]byte(k)); !ok || g != l {
			t.Fatalf("get %q", k)
		}
	}
}

func TestGlob(t *testing.T) {
	cases := []struct {
		pat, key string
		want     bool
	}{
		{"*", "", true}, {"*", "anything/at all", true},
		{"user:*", "user:42", true}, {"user:*", "usr:42", false},
		{"user:?", "user:4", true}, {"user:?", "user:42", false},
		{"*:email", "user:42:email", true}, {"*:email", "user:42:emails", false},
		{"a*b*c", "aXXbYYc", true}, {"a*b*c", "aXXbYY", false}, {"a*b*c", "abc", true},
		{"[a-c]x", "bx", true}, {"[a-c]x", "dx", false}, {"[^a-c]x", "dx", true}, {"[!a-c]x", "ax", false},
		{`a\*b`, "a*b", true}, {`a\*b`, "aXb", false},
		{"h?llo", "héllo", true}, {"caf[é]", "café", true},
		{"**a**", "xxaxx", true}, {"a*a*a*a*a*a*a*b", strings.Repeat("a", 60), false},
		{"exact", "exact", true}, {"exact", "exactx", false},
	}
	for _, c := range cases {
		g, err := compile(c.pat)
		if err != nil {
			t.Fatalf("%q: %v", c.pat, err)
		}
		if got := g.match([]byte(c.key)); got != c.want {
			t.Errorf("match(%q,%q)=%v want %v", c.pat, c.key, got, c.want)
		}
	}
	for _, bad := range []string{"[abc", `abc\`, "[z-a]", "[]", "x[^]"} {
		if _, err := compile(bad); err == nil {
			t.Errorf("%q should be malformed", bad)
		}
	}
	g, _ := compile(`us\*er:42:*`)
	if string(g.prefix) != "us*er:42:" || g.exact {
		t.Errorf("prefix %q", g.prefix)
	}
}
