// Package trailer appends and reads a small JSON config blob at the end of an
// executable. This is how the server "generates" per-organization installers
// without needing a compiler at runtime: it streams the prebuilt agent binary
// and appends the enrollment config. Windows PE and Linux ELF loaders both
// ignore trailing overlay data.
//
// Layout: <binary><json payload><uint64 LE payload length><8-byte magic>
package trailer

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
)

var magic = []byte("OCEANIC1")

const footerLen = 16

// ErrNoTrailer is returned when the file has no embedded config.
var ErrNoTrailer = errors.New("no embedded config")

// Build returns the bytes to append after a binary to embed v.
func Build(v any) ([]byte, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.Write(payload)
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], uint64(len(payload)))
	buf.Write(n[:])
	buf.Write(magic)
	return buf.Bytes(), nil
}

// Read extracts the embedded config from the file at path into v.
func Read(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	if size < footerLen {
		return ErrNoTrailer
	}
	foot := make([]byte, footerLen)
	if _, err := f.ReadAt(foot, size-footerLen); err != nil {
		return err
	}
	if !bytes.Equal(foot[8:], magic) {
		return ErrNoTrailer
	}
	n := int64(binary.LittleEndian.Uint64(foot[:8]))
	if n <= 0 || n > 1<<20 || n > size-footerLen {
		return ErrNoTrailer
	}
	payload := make([]byte, n)
	if _, err := f.ReadAt(payload, size-footerLen-n); err != nil && err != io.EOF {
		return err
	}
	return json.Unmarshal(payload, v)
}
