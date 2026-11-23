package codec

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// MaxMessageSize bounds the frame a Decoder will read. Configure it before
// starting concurrent decoding; do not mutate it while Decoder calls are active.
var MaxMessageSize = 1 << 28

// An Encoder writes a stream of values to an io.Writer, each prefixed with
// its length, like json.Encoder. It reuses one buffer between calls.
type Encoder struct {
	// Evolvable makes Encode write messages that later versions of the type
	// can still read after fields are added (see MarshalEvolvable).
	Evolvable bool

	w   io.Writer
	buf []byte
}

// NewEncoder returns an Encoder writing to w.
func NewEncoder(w io.Writer) *Encoder { return &Encoder{w: w} }

// Encode writes the next value.
func (e *Encoder) Encode(v any) error {
	const room = binary.MaxVarintLen64
	if cap(e.buf) < room {
		e.buf = make([]byte, room, 512)
	}
	append := Append
	if e.Evolvable {
		append = AppendEvolvable
	}
	buf, err := append(e.buf[:room], v)
	if err != nil {
		return err
	}
	e.buf = buf[:0]
	var hdr [room]byte
	hn := binary.PutUvarint(hdr[:], uint64(len(buf)-room))
	start := room - hn
	copy(buf[start:], hdr[:hn])
	var n int
	n, err = e.w.Write(buf[start:])
	if err == nil && n != len(buf[start:]) {
		err = io.ErrShortWrite
	}
	return err
}

// A Decoder reads a stream written by an Encoder, like json.Decoder.
type Decoder struct {
	r   *bufio.Reader
	buf []byte
}

// NewDecoder returns a Decoder reading from r.
func NewDecoder(r io.Reader) *Decoder { return &Decoder{r: bufio.NewReader(r)} }

// Decode reads the next value into v (a pointer, as for Unmarshal). It
// returns io.EOF when the stream ends cleanly between values.
func (d *Decoder) Decode(v any) error {
	n, err := binary.ReadUvarint(d.r)
	if err != nil {
		return err // io.EOF at a value boundary
	}
	if n > uint64(MaxMessageSize) {
		return fmt.Errorf("codec: message of %d bytes exceeds MaxMessageSize", n)
	}
	if uint64(cap(d.buf)) < n {
		d.buf = make([]byte, n)
	}
	d.buf = d.buf[:n]
	if _, err := io.ReadFull(d.r, d.buf); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	return Unmarshal(d.buf, v) // v receives copies, so the buffer is reused safely
}

// More reports whether another value is available in the stream.
func (d *Decoder) More() bool {
	_, err := d.r.Peek(1)
	return err == nil
}
