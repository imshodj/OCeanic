package trailer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bin")
	tail, err := Build(map[string]string{"server_url": "https://rmm.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, append([]byte("MZ fake binary contents"), tail...), 0o644); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := Read(p, &got); err != nil {
		t.Fatal(err)
	}
	if got["server_url"] != "https://rmm.example.com" {
		t.Fatalf("got %v", got)
	}
}

func TestNoTrailer(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bin")
	os.WriteFile(p, []byte("plain binary without config at all"), 0o644)
	var v any
	if err := Read(p, &v); err != ErrNoTrailer {
		t.Fatalf("want ErrNoTrailer, got %v", err)
	}
}
