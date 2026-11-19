package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"

	"github.com/oarkflow/codec"
)

type record struct {
	ID     uint64
	Name   string
	Active bool
	Scores []int32
	Labels map[string]string
	Flags  [3]bool
	Maybe  *string
	Groups [][]string
	Peers  []*point
	Meta   any
}

type point struct {
	TS   int64
	Host string
}

type metric struct {
	Counter  int64
	Category uint64
	Ratio    float64
	Name     string
	Flag     bool
}

type oldUser struct {
	Name string
	Age  uint32
}

type olderUser struct{ Name string }

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--emit" {
		emit()
		return
	}
	var messages map[string]string
	if err := json.NewDecoder(os.Stdin).Decode(&messages); err != nil {
		fatal(err)
	}
	var trailing any
	if err := json.NewDecoder(os.Stdin).Decode(&trailing); err != io.EOF {
		fatal(fmt.Errorf("expected one JSON input: %v", err))
	}

	var gotRecord record
	decode(messages["record"], &gotRecord)
	maybe := "linked"
	wantRecord := record{
		ID: 9007199254740993, Name: "Ada", Active: true,
		Scores: []int32{-2, 0, 3}, Labels: map[string]string{"a": "first", "z": "last"},
		Flags: [3]bool{true, false, true}, Maybe: &maybe,
		Groups: [][]string{{"x", "", "y"}, nil}, Peers: []*point{{9, "db"}, nil},
		Meta: map[string]any{"role": "admin", "count": int64(7), "flags": []any{true, false}, "raw": []byte{1, 2}},
	}
	if !reflect.DeepEqual(gotRecord, wantRecord) {
		fatal(fmt.Errorf("record mismatch: %#v", gotRecord))
	}
	streamBytes, err := hex.DecodeString(messages["stream"])
	if err != nil {
		fatal(err)
	}
	stream := codec.NewDecoder(bytes.NewReader(streamBytes))
	for i := 0; i < 2; i++ {
		var streamed record
		if err := stream.Decode(&streamed); err != nil || !reflect.DeepEqual(streamed, wantRecord) {
			fatal(fmt.Errorf("stream item %d mismatch: %#v (%v)", i, streamed, err))
		}
	}
	if stream.More() {
		fatal(fmt.Errorf("unexpected extra stream item"))
	}

	var gotPoints []point
	decode(messages["points"], &gotPoints)
	wantPoints := []point{{1000, "web"}, {1001, "web"}, {1002, "db"}, {1003, "web"}}
	if !reflect.DeepEqual(gotPoints, wantPoints) {
		fatal(fmt.Errorf("points mismatch: %#v", gotPoints))
	}

	var gotUser oldUser
	decode(messages["evolved"], &gotUser)
	if gotUser != (oldUser{"Ada", 36}) {
		fatal(fmt.Errorf("evolved value mismatch: %#v", gotUser))
	}
	fmt.Println("Go decoded all TypeScript fixtures")
}

func emit() {
	maybe := "linked"
	rec := record{
		ID: 9007199254740993, Name: "Ada", Active: true,
		Scores: []int32{-2, 0, 3}, Labels: map[string]string{"a": "first", "z": "last"},
		Flags: [3]bool{true, false, true}, Maybe: &maybe,
		Groups: [][]string{{"x", "", "y"}, nil}, Peers: []*point{{9, "db"}, nil},
		Meta: map[string]any{"role": "admin", "count": int64(7), "flags": []any{true, false}, "raw": []byte{1, 2}},
	}
	recordPlain, err := codec.Marshal(&rec)
	if err != nil {
		fatal(err)
	}
	var stream bytes.Buffer
	streamEncoder := codec.NewEncoder(&stream)
	streamEncoder.Evolvable = true
	if err := streamEncoder.Encode(&rec); err != nil {
		fatal(err)
	}
	if err := streamEncoder.Encode(&rec); err != nil {
		fatal(err)
	}
	points, err := codec.Marshal(&[]point{{1000, "web"}, {1001, "web"}, {1002, "db"}, {1003, "web"}})
	if err != nil {
		fatal(err)
	}
	metrics := make([]metric, 100)
	for i := range metrics {
		category := uint64(1)
		if i%3 == 1 {
			category = 1 << 40
		} else if i%3 == 2 {
			category = 1<<40 + 10
		}
		metrics[i] = metric{
			Counter:  1_000_000 + int64(i)*100 + int64(i%2)*2,
			Category: category, Ratio: float64(i%2) + 1.5,
			Name: []string{"edge", "core"}[i%2], Flag: i%2 == 0,
		}
	}
	metricBytes, err := codec.Marshal(&metrics)
	if err != nil {
		fatal(err)
	}
	evolved, err := codec.MarshalEvolvable(&oldUser{"Ada", 36})
	if err != nil {
		fatal(err)
	}
	older, err := codec.MarshalEvolvable(&olderUser{"Ada"})
	if err != nil {
		fatal(err)
	}
	out := map[string]string{
		"record":  hex.EncodeToString(recordPlain),
		"stream":  hex.EncodeToString(stream.Bytes()),
		"points":  hex.EncodeToString(points),
		"metrics": hex.EncodeToString(metricBytes),
		"evolved": hex.EncodeToString(evolved),
		"older":   hex.EncodeToString(older),
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		fatal(err)
	}
}

func decode(text string, out any) {
	b, err := hex.DecodeString(text)
	if err != nil {
		fatal(err)
	}
	if err := codec.Unmarshal(b, out); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
