//go:build linux

package agent

import (
	"os"
	"testing"
	"time"
)

// Run with: DISPLAY=:99 go test -run TestDesktopTiming -v ./internal/agent/
func TestDesktopTiming(t *testing.T) {
	if os.Getenv("DISPLAY") == "" {
		t.Skip("needs an X display")
	}
	scr, err := openScreen()
	if err != nil {
		t.Skip(err)
	}
	defer scr.Close()
	enc := &frameEncoder{quality: 60}
	for i := 0; i < 5; i++ {
		t0 := time.Now()
		img, err := scr.Capture(1)
		if err != nil {
			t.Fatal(err)
		}
		t1 := time.Now()
		enc.prev = nil
		msg := enc.encode(img)
		t2 := time.Now()
		t.Logf("capture %v  full-encode %v  (%d KB)", t1.Sub(t0), t2.Sub(t1), len(msg)/1024)
	}
}
