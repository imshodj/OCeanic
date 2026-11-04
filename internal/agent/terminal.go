package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/imshodj/oceanic/internal/protocol"
)

// ptyProc is a shell attached to a pseudo-terminal (creack/pty on Unix,
// ConPTY on Windows).
type ptyProc interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Resize(cols, rows int) error
	Wait() (int, error)
	Close() error // kills the shell and releases the PTY
}

func clampSize(cols, rows int) (int, int) {
	if cols < 20 || cols > 500 {
		cols = 120
	}
	if rows < 5 || rows > 300 {
		rows = 32
	}
	return cols, rows
}

// runTerminal dials back to the server and bridges a local PTY shell to it.
func (a *Agent) runTerminal(ctx context.Context, j protocol.Job) {
	conn, err := dialSession(ctx, a.c, j.ID)
	if err != nil {
		log.Printf("terminal %s: dial: %v", j.ID, err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	ctrl := func(m protocol.TermControl) {
		b, _ := json.Marshal(m)
		wctx, c := context.WithTimeout(ctx, 5*time.Second)
		conn.Write(wctx, websocket.MessageText, b)
		c()
	}

	cols, rows := clampSize(j.Cols, j.Rows)
	p, err := startPTY(j.Shell, cols, rows)
	if err != nil {
		ctrl(protocol.TermControl{Type: "error", Message: "could not start " + j.Shell + ": " + err.Error()})
		conn.Close(websocket.StatusNormalClosure, "")
		return
	}
	log.Printf("terminal %s: started %s (%dx%d)", j.ID, j.Shell, cols, rows)

	sctx, stop := context.WithCancel(ctx)
	defer stop()
	var once sync.Once
	closePTY := func() { once.Do(func() { p.Close() }) }
	defer closePTY()

	// PTY -> server
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := p.Read(buf)
			if n > 0 {
				if werr := conn.Write(sctx, websocket.MessageBinary, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// server -> PTY
	go func() {
		defer stop()
		for {
			typ, data, err := conn.Read(sctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				if _, err := p.Write(data); err != nil {
					return
				}
				continue
			}
			var m protocol.TermControl
			if json.Unmarshal(data, &m) == nil && m.Type == "resize" {
				c, r := clampSize(m.Cols, m.Rows)
				p.Resize(c, r)
			}
		}
	}()

	// Shell exit -> tell the browser
	exited := make(chan int, 1)
	go func() {
		code, _ := p.Wait()
		exited <- code
	}()

	select {
	case code := <-exited:
		// Give the reader a moment to flush the last output, then release the PTY.
		select {
		case <-outDone:
		case <-time.After(300 * time.Millisecond):
		}
		closePTY()
		ctrl(protocol.TermControl{Type: "exit", Code: code})
		conn.Close(websocket.StatusNormalClosure, "shell exited")
	case <-sctx.Done():
		// Browser/server went away: kill the shell.
		closePTY()
	}
	log.Printf("terminal %s: closed", j.ID)
}

var errNoPTY = errors.New("pseudo-terminals are not supported on this system")
