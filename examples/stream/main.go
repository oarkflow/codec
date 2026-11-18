// stream: length-prefixed messages over a connection. Strata messages are not
// self-delimiting, so a transport frames them; here a uvarint length prefix.
package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"sync"

	"github.com/oarkflow/codec"
)

type Event struct {
	Seq     uint64
	Kind    string
	Payload []byte
}

func writeMsg(w io.Writer, buf []byte, v any) ([]byte, error) {
	// Encode first, then write the uvarint length followed by the bytes.
	buf, err := codec.Append(buf[:0], v)
	if err != nil {
		return buf, err
	}
	var hdr [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(hdr[:], uint64(len(buf)))
	if _, err := w.Write(hdr[:n]); err != nil {
		return buf, err
	}
	_, err = w.Write(buf)
	return buf, err
}

func readMsg(r *bufio.Reader, buf []byte, v any) ([]byte, error) {
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return buf, err
	}
	if n > 1<<20 {
		return buf, fmt.Errorf("frame too large: %d", n)
	}
	if uint64(cap(buf)) < n {
		buf = make([]byte, n)
	}
	buf = buf[:n]
	if _, err := io.ReadFull(r, buf); err != nil {
		return buf, err
	}
	return buf, codec.Unmarshal(buf, v) // v gets copies, so buf can be reused
}

func main() {
	client, server := net.Pipe()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer server.Close()
		var buf []byte
		for i := uint64(1); i <= 5; i++ {
			ev := Event{Seq: i, Kind: "tick", Payload: []byte(fmt.Sprintf("payload-%d", i))}
			var err error
			if buf, err = writeMsg(server, buf, &ev); err != nil {
				log.Print(err)
				return
			}
		}
	}()

	r := bufio.NewReader(client)
	var ev Event
	var buf []byte
	for {
		var err error
		buf, err = readMsg(r, buf, &ev)
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("got seq=%d kind=%s payload=%s\n", ev.Seq, ev.Kind, ev.Payload)
	}
	wg.Wait()
}
