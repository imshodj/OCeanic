package agent

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestStdioConnRoundTrip(t *testing.T) {
	r, w := io.Pipe()
	a, b := newStdioConn(r, io.Discard), newStdioConn(eofReader{}, w)
	go func() {
		b.Write(context.Background(), websocket.MessageText, []byte(`{"t":"cfg"}`))
		b.Write(context.Background(), websocket.MessageBinary, make([]byte, 300000))
	}()
	typ, data, err := a.Read(context.Background())
	if err != nil || typ != websocket.MessageText || string(data) != `{"t":"cfg"}` {
		t.Fatalf("text frame: %v %v %q", typ, err, data)
	}
	typ, data, err = a.Read(context.Background())
	if err != nil || typ != websocket.MessageBinary || len(data) != 300000 {
		t.Fatalf("binary frame: %v %v %d", typ, err, len(data))
	}
}

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

// helperOverStdio is used by TestDesktopHelperStdio to check a live helper.
func readUntilFrame(t *testing.T, c *stdioConn) (size map[string]any, frame []byte) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		typ, data, err := c.Read(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if typ == websocket.MessageText {
			json.Unmarshal(data, &size)
			if size["type"] == "error" {
				t.Fatalf("helper error: %v", size["message"])
			}
			continue
		}
		return size, data
	}
	t.Fatal("no frame")
	return
}
