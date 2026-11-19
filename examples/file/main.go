// file: save and load Strata data on disk. Two patterns:
//  1. one value per file (write the bytes, read them back);
//  2. an append-only log of records, each prefixed with its length, read back
//     through one reusable input buffer while each decoded record is independent.
package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/oarkflow/codec"
)

type Config struct {
	Name    string
	Port    uint16
	Debug   bool
	Servers []string
	Limits  []int64
}

type LogRecord struct {
	Seq   uint64
	Level string
	Msg   string
}

func saveConfig(path string, c *Config) error {
	data, err := codec.Marshal(c)
	if err != nil {
		return err
	}
	// write to a temp file and rename so readers never see a partial file
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := codec.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func appendRecords(path string, recs []LogRecord) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	var buf []byte
	for i := range recs {
		if buf, err = codec.Append(buf[:0], &recs[i]); err != nil {
			return err
		}
		var hdr [binary.MaxVarintLen64]byte
		n := binary.PutUvarint(hdr[:], uint64(len(buf)))
		if _, err := w.Write(hdr[:n]); err != nil {
			return err
		}
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}
	return w.Flush()
}

func readRecords(path string, fn func(*LogRecord)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var buf []byte
	for {
		n, err := binary.ReadUvarint(r)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if n > 1<<24 {
			return fmt.Errorf("record too large: %d", n)
		}
		if uint64(cap(buf)) < n {
			buf = make([]byte, n)
		}
		buf = buf[:n]
		if _, err := io.ReadFull(r, buf); err != nil {
			return err // truncated tail
		}
		var rec LogRecord // fresh value per record: safe to retain in fn
		if err := codec.Unmarshal(buf, &rec); err != nil {
			return err
		}
		fn(&rec)
	}
}

func main() {
	dir, err := os.MkdirTemp("", "strata-example")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	cfgPath := filepath.Join(dir, "config.strata")
	cfg := &Config{Name: "api", Port: 8080, Debug: true,
		Servers: []string{"10.0.0.1", "10.0.0.2"}, Limits: []int64{100, 1000, 10000}}
	if err := saveConfig(cfgPath, cfg); err != nil {
		log.Fatal(err)
	}
	st, err := os.Stat(cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	loaded, err := loadConfig(cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("config: %d bytes on disk -> %+v\n", st.Size(), *loaded)

	logPath := filepath.Join(dir, "app.log.strata")
	if err := appendRecords(logPath, []LogRecord{{1, "info", "started"}, {2, "warn", "slow query"}}); err != nil {
		log.Fatal(err)
	}
	if err := appendRecords(logPath, []LogRecord{{3, "error", "connection reset"}}); err != nil { // append later
		log.Fatal(err)
	}
	if err := readRecords(logPath, func(r *LogRecord) {
		fmt.Printf("record seq=%d level=%s msg=%q\n", r.Seq, r.Level, r.Msg)
	}); err != nil {
		log.Fatal(err)
	}

	// Corrupt files are reported, not panicked on.
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, data[:len(data)/2], 0o644); err != nil {
		log.Fatal(err)
	}
	_, err = loadConfig(cfgPath)
	fmt.Println("truncated file:", err)
}
