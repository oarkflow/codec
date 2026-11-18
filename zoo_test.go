package codec

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// A property test over randomly generated types and values: whatever shape the
// data has, every representation must round-trip it.

type zoo struct {
	r *rand.Rand
}

var zooScalars = []reflect.Type{
	reflect.TypeFor[bool](), reflect.TypeFor[int8](), reflect.TypeFor[int16](), reflect.TypeFor[int32](), reflect.TypeFor[int64](),
	reflect.TypeFor[int](), reflect.TypeFor[uint8](), reflect.TypeFor[uint16](), reflect.TypeFor[uint32](), reflect.TypeFor[uint64](),
	reflect.TypeFor[float32](), reflect.TypeFor[float64](), reflect.TypeFor[string](),
}

var zooKeys = []reflect.Type{
	reflect.TypeFor[string](), reflect.TypeFor[int64](), reflect.TypeFor[int32](), reflect.TypeFor[uint16](), reflect.TypeFor[bool](),
}

func (z *zoo) typ(depth int) reflect.Type {
	if depth <= 0 || z.r.Intn(4) == 0 {
		if z.r.Intn(8) == 0 {
			return reflect.TypeFor[any]()
		}
		return zooScalars[z.r.Intn(len(zooScalars))]
	}
	switch z.r.Intn(8) {
	case 0, 1:
		return reflect.SliceOf(z.typ(depth - 1))
	case 2:
		return reflect.ArrayOf(1+z.r.Intn(3), z.typ(depth-1))
	case 3:
		return reflect.MapOf(zooKeys[z.r.Intn(len(zooKeys))], z.typ(depth-1))
	case 4:
		el := z.typ(depth - 1)
		for el.Kind() == reflect.Pointer { // **T is not realistic data
			el = el.Elem()
		}
		return reflect.PointerTo(el)
	case 5:
		return reflect.TypeFor[[]byte]()
	default:
		n := 1 + z.r.Intn(6)
		fs := make([]reflect.StructField, n)
		for i := range fs {
			fs[i] = reflect.StructField{Name: fmt.Sprintf("F%d", i), Type: z.typ(depth - 1)}
		}
		return reflect.StructOf(fs)
	}
}

var zooStrings = []string{"", "a", "hello world", "x;y", "~", "-", "#c", "\"q\"", "a.b", "tab\there", "line\nbreak", "日本語", "0", "true", "1.5", "0xff", " lead", "trail ", "+", "^", "end", "k=v"}

func (z *zoo) str() string {
	if z.r.Intn(12) == 0 {
		b := make([]byte, 1+z.r.Intn(5))
		for i := range b {
			b[i] = byte(z.r.Intn(256)) // may be invalid UTF-8
		}
		return string(b)
	}
	return zooStrings[z.r.Intn(len(zooStrings))]
}

func (z *zoo) dyn(depth int) any {
	if depth <= 0 {
		switch z.r.Intn(6) {
		case 0:
			return nil
		case 1:
			return z.r.Intn(2) == 0
		case 2:
			return z.r.Int63n(1000) - 500
		case 3:
			return uint64(1<<63) + uint64(z.r.Intn(100))
		case 4:
			return math.Round(z.r.NormFloat64()*1000) / 8
		default:
			return z.str()
		}
	}
	switch z.r.Intn(4) {
	case 0:
		n := z.r.Intn(4)
		l := make([]any, n)
		for i := range l {
			l[i] = z.dyn(depth - 1)
		}
		return l
	case 1:
		n := z.r.Intn(4)
		m := map[string]any{}
		for i := 0; i < n; i++ {
			m[z.str()] = z.dyn(depth - 1)
		}
		return m
	case 2:
		b := make([]byte, z.r.Intn(4))
		z.r.Read(b)
		return b
	}
	return z.dyn(0)
}

func (z *zoo) val(t reflect.Type, depth int) reflect.Value {
	v := reflect.New(t).Elem()
	switch t.Kind() {
	case reflect.Bool:
		v.SetBool(z.r.Intn(2) == 0)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		x := z.r.Int63n(1<<uint(min(t.Bits(), 62))) - 1<<uint(min(t.Bits(), 62)-1)
		if z.r.Intn(4) == 0 {
			x = 0
		}
		v.SetInt(x)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		x := z.r.Uint64() >> uint(64-min(t.Bits(), 64))
		if z.r.Intn(4) == 0 {
			x = 0
		}
		v.SetUint(x)
	case reflect.Float32:
		v.SetFloat(float64(float32(math.Round(z.r.NormFloat64()*1000) / 16)))
	case reflect.Float64:
		v.SetFloat(math.Round(z.r.NormFloat64()*1e6) / 64)
	case reflect.String:
		v.SetString(z.str())
	case reflect.Interface:
		if x := z.dyn(2); x != nil {
			v.Set(reflect.ValueOf(x))
		}
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			b := make([]byte, z.r.Intn(6))
			z.r.Read(b)
			v.SetBytes(b)
			break
		}
		n := z.r.Intn(6)
		if z.r.Intn(8) == 0 {
			n = 5 + z.r.Intn(30) // exercise the columnar / bulk layouts
		}
		s := reflect.MakeSlice(t, n, n)
		for i := 0; i < n; i++ {
			s.Index(i).Set(z.val(t.Elem(), depth-1))
		}
		v.Set(s)
	case reflect.Array:
		for i := 0; i < t.Len(); i++ {
			v.Index(i).Set(z.val(t.Elem(), depth-1))
		}
	case reflect.Map:
		n := z.r.Intn(4)
		m := reflect.MakeMap(t)
		for i := 0; i < n; i++ {
			m.SetMapIndex(z.val(t.Key(), 0), z.val(t.Elem(), depth-1))
		}
		v.Set(m)
	case reflect.Pointer:
		if z.r.Intn(3) != 0 {
			p := reflect.New(t.Elem())
			p.Elem().Set(z.val(t.Elem(), depth-1))
			v.Set(p)
		}
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			v.Field(i).Set(z.val(t.Field(i).Type, depth-1))
		}
	}
	return v
}

// zeroLike reports whether v is a zero value, counting empty slices and maps:
// the codec stores "zero" as "absent".
func zeroLike(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Slice, reflect.Map:
		return v.Len() == 0
	case reflect.Pointer:
		return v.IsNil() || zeroLike(v.Elem())
	case reflect.Interface:
		return v.IsNil()
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if !zeroLike(v.Field(i)) {
				return false
			}
		}
		return true
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if !zeroLike(v.Index(i)) {
				return false
			}
		}
		return true
	}
	return v.IsZero()
}

// same compares like DeepEqual but treats nil and empty slices/maps as equal and
// a nil pointer as a pointer to a zero value.
func same(a, b reflect.Value) bool {
	if a.Kind() != b.Kind() {
		return false
	}
	switch a.Kind() {
	case reflect.Pointer:
		az, bz := zeroLike(a), zeroLike(b)
		if az || bz {
			return az && bz
		}
		return same(a.Elem(), b.Elem())
	case reflect.Interface:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() && b.IsNil()
		}
		return same(a.Elem(), b.Elem())
	case reflect.Slice:
		if a.Len() != b.Len() {
			return false
		}
		for i := 0; i < a.Len(); i++ {
			if !same(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Array:
		for i := 0; i < a.Len(); i++ {
			if !same(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Map:
		if a.Len() != b.Len() {
			return false
		}
		for _, k := range a.MapKeys() {
			bv := b.MapIndex(k)
			if !bv.IsValid() || !same(a.MapIndex(k), bv) {
				return false
			}
		}
		return true
	case reflect.Struct:
		for i := 0; i < a.NumField(); i++ {
			if !same(a.Field(i), b.Field(i)) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a.Interface(), b.Interface())
}

func TestZooAnyShape(t *testing.T) {
	n := 4000
	if testing.Short() {
		n = 400
	}
	seed, depth := int64(20261118), 5
	if s := os.Getenv("CODEC_ZOO_SEED"); s != "" {
		seed, _ = strconv.ParseInt(s, 10, 64)
	}
	if s := os.Getenv("CODEC_ZOO_N"); s != "" {
		n, _ = strconv.Atoi(s)
	}
	if s := os.Getenv("CODEC_ZOO_DEPTH"); s != "" {
		depth, _ = strconv.Atoi(s)
	}
	z := &zoo{r: rand.New(rand.NewSource(seed))}
	fails := map[string]int{}
	var first = map[string]string{}
	note := func(kind string, typ reflect.Type, detail string) {
		fails[kind]++
		if _, ok := first[kind]; !ok {
			first[kind] = fmt.Sprintf("%v\n   %s", typ, detail)
		}
	}
	checked := 0
	for i := 0; i < n; i++ {
		typ := z.typ(depth)
		if typ.Kind() == reflect.Pointer { // a pointer root is followed and allocated by design
			typ = reflect.StructOf([]reflect.StructField{{Name: "F0", Type: typ}})
		}
		in := z.val(typ, depth)
		checked++
		ptr := reflect.New(typ)
		ptr.Elem().Set(in)

		// binary
		bin, err := Marshal(ptr.Interface())
		if err != nil {
			note("binary-marshal", typ, err.Error())
			continue
		}
		out := reflect.New(typ)
		if err := Unmarshal(bin, out.Interface()); err != nil {
			note("binary-unmarshal", typ, err.Error())
		} else if !same(in, out.Elem()) {
			note("binary-mismatch", typ, fmt.Sprintf("%#v\n   %#v", in.Interface(), out.Elem().Interface()))
		}

		// evolvable
		ev, err := MarshalEvolvable(ptr.Interface())
		out2 := reflect.New(typ)
		if err != nil {
			note("evolvable-marshal", typ, err.Error())
		} else if err := Unmarshal(ev, out2.Interface()); err != nil {
			note("evolvable-unmarshal", typ, err.Error())
		} else if !same(in, out2.Elem()) {
			note("evolvable-mismatch", typ, "")
		}

		// text
		txt, err := MarshalText(ptr.Interface())
		if err != nil {
			note("text-marshal", typ, err.Error())
			continue
		}
		out3 := reflect.New(typ)
		if err := UnmarshalText(txt, out3.Interface()); err != nil {
			note("text-unmarshal", typ, fmt.Sprintf("%v\n%s", err, clip(string(txt))))
		} else if !same(in, out3.Elem()) {
			note("text-mismatch", typ, fmt.Sprintf("%s\n   %#v\n   %#v", clip(string(txt)), in.Interface(), out3.Elem().Interface()))
		} else {
			// a pointer to a zero value prints as ~ and reads back as nil, so the
			// first print can differ from the second; from then on it must be stable
			txt2, _ := MarshalText(out3.Interface())
			out4 := reflect.New(typ)
			if err := UnmarshalText(txt2, out4.Interface()); err != nil {
				note("text-unmarshal-2", typ, err.Error())
			} else if txt3, _ := MarshalText(out4.Interface()); string(txt3) != string(txt2) {
				note("text-unstable", typ, clip(string(txt2))+"\n---\n"+clip(string(txt3)))
			}
		}

		// binary -> text -> binary is byte-identical (dynamic values excepted: see below)
		if !strings.Contains(typ.String(), "interface") {
			if tx, err := ToText(bin, ptr.Interface()); err != nil {
				note("bin-to-text", typ, err.Error())
			} else if back, err := FromText(tx, ptr.Interface()); err != nil {
				note("text-to-bin", typ, fmt.Sprintf("%v\n%s", err, clip(string(tx))))
			} else {
				// offsets are sized adaptively, so equal data can encode to different
				// bytes: compare what the two messages decode to, strictly
				a, b := reflect.New(typ), reflect.New(typ)
				if Unmarshal(bin, a.Interface()) != nil || Unmarshal(back, b.Interface()) != nil || !reflect.DeepEqual(a.Elem().Interface(), b.Elem().Interface()) {
					note("bin-text-bin-values", typ, clip(string(tx)))
				}
			}
		}
	}
	t.Logf("checked %d random types/values", checked)
	for k, c := range fails {
		t.Errorf("%s: %d failures; first on type %s", k, c, first[k])
	}
}

func clip(s string) string {
	if len(s) > 600 {
		return s[:600] + "..."
	}
	return s
}
