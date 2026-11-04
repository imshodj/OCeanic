package agent

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/coder/websocket"
)

// Runs a real "desktop-helper --stdio" process (the macOS code path's
// transport) against an X display. Needs OCEANIC_TEST_AGENT=<binary> and DISPLAY.
func TestDesktopHelperStdio(t *testing.T) {
	bin := os.Getenv("OCEANIC_TEST_AGENT")
	if bin == "" || os.Getenv("DISPLAY") == "" {
		t.Skip("set OCEANIC_TEST_AGENT and DISPLAY")
	}
	cmd := exec.Command(bin, "desktop-helper", "--stdio")
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	c := newStdioConn(out, in)
	size, frame := readUntilFrame(t, c)
	t.Logf("size=%v first frame=%d bytes", size, len(frame))
	if size["type"] != "size" || frame[0] != 1 {
		t.Fatalf("unexpected: %v %x", size, frame[:1])
	}
	// ack + switch to 50% scale; the helper must resend a smaller size.
	c.Write(context.Background(), websocket.MessageBinary, []byte{1})
	c.Write(context.Background(), websocket.MessageText, []byte(`{"t":"cfg","q":50,"scale":0.5}`))
	size, frame = readUntilFrame(t, c)
	t.Logf("after cfg: size=%v frame=%d bytes", size, len(frame))
	if size["w"] == nil {
		t.Fatal("no resize after cfg")
	}
	in.Close()
	if err := cmd.Wait(); err != nil {
		t.Logf("helper exit: %v", err)
	}
}
