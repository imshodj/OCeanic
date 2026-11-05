package agent

// Remote desktop: shared session logic.
//
// A desktop session runs in a helper process ("oceanic-agent desktop-helper
// <sid>") that the agent service launches inside the interactive session (on
// Windows the service itself lives in session 0 and cannot see the screen).
// The helper dials the server over a WebSocket, just like the terminal.
//
// Wire format (agent -> browser):
//   binary  frame:  0x01 | u16 tileCount | tileCount × (u16 x, u16 y, u16 w, u16 h, u32 len, JPEG bytes)
//   text    JSON:   {"type":"size","w":..,"h":..} | {"type":"error"|"info","message":..}
// Browser -> agent:
//   binary  0x01 = ack (frame drawn; flow control)
//   text    JSON input events, see desktopInput.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/imshodj/oceanic/internal/protocol"
)

// desktopScreen is implemented per platform. All methods are called from a
// single OS-locked goroutine (Windows requires capture and input to happen on
// a thread attached to the current input desktop).
type desktopScreen interface {
	// Capture grabs the whole (virtual) screen scaled by `scale` into an
	// RGBA image. It may return a different size than last time.
	Capture(scale float64) (*image.RGBA, error)
	// FullSize is the unscaled size of the captured area.
	FullSize() (w, h int)
	// Input injects one event. fx/fy are 0..1 fractions of the full screen.
	Input(ev desktopInput) error
	Close()
}

type desktopInput struct {
	T     string  `json:"t"` // mm, md, mu, wh, kd, ku, text, cad, cfg, refresh
	X     float64 `json:"x,omitempty"`
	Y     float64 `json:"y,omitempty"`
	B     int     `json:"b,omitempty"` // 0 left, 1 middle, 2 right
	DX    int     `json:"dx,omitempty"`
	DY    int     `json:"dy,omitempty"`
	Code  string  `json:"code,omitempty"` // KeyboardEvent.code
	S     string  `json:"s,omitempty"`
	Q     int     `json:"q,omitempty"`     // JPEG quality
	Scale float64 `json:"scale,omitempty"` // 0 = auto
}

const (
	tileSize     = 64
	maxInFlight  = 2
	minFrameGap  = 66 * time.Millisecond // ~15 fps ceiling
	idleFrameGap = 150 * time.Millisecond
)

// ---------- frame diff + encode ----------

type tile struct {
	r    image.Rectangle
	data []byte
}

type frameEncoder struct {
	prev    []byte
	w, h    int
	quality int
}

// changedStrips compares cur against the previous frame in tileSize blocks and
// merges horizontally adjacent changed blocks into strips.
func (e *frameEncoder) changedStrips(cur *image.RGBA) []image.Rectangle {
	w, h := cur.Rect.Dx(), cur.Rect.Dy()
	full := e.prev == nil || e.w != w || e.h != h
	var out []image.Rectangle
	for ty := 0; ty < h; ty += tileSize {
		th := min(tileSize, h-ty)
		runStart := -1
		for tx := 0; ; tx += tileSize {
			if tx >= w { // end of row: close any open run (w needn't be a multiple of tileSize)
				if runStart >= 0 {
					out = append(out, image.Rect(runStart, ty, w, ty+th))
				}
				break
			}
			changed := full
			if !full {
				tw := min(tileSize, w-tx)
				for y := ty; y < ty+th; y++ {
					off := y*cur.Stride + tx*4
					if !bytes.Equal(cur.Pix[off:off+tw*4], e.prev[off:off+tw*4]) {
						changed = true
						break
					}
				}
			}
			if changed && runStart < 0 {
				runStart = tx
			} else if !changed && runStart >= 0 {
				out = append(out, image.Rect(runStart, ty, tx, ty+th))
				runStart = -1
			}
		}
	}
	return out
}

// encode returns the binary frame message, or nil if nothing changed.
func (e *frameEncoder) encode(cur *image.RGBA) []byte {
	rects := e.changedStrips(cur)
	w, h := cur.Rect.Dx(), cur.Rect.Dy()
	if e.prev == nil || len(e.prev) != len(cur.Pix) {
		e.prev = make([]byte, len(cur.Pix))
	}
	copy(e.prev, cur.Pix)
	e.w, e.h = w, h
	if len(rects) == 0 {
		return nil
	}

	tiles := make([]tile, len(rects))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for i, r := range rects {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, r image.Rectangle) {
			defer wg.Done()
			defer func() { <-sem }()
			var buf bytes.Buffer
			jpeg.Encode(&buf, cur.SubImage(r), &jpeg.Options{Quality: e.quality})
			tiles[i] = tile{r: r, data: buf.Bytes()}
		}(i, r)
	}
	wg.Wait()

	size := 3
	for _, t := range tiles {
		size += 12 + len(t.data)
	}
	msg := make([]byte, 0, size)
	msg = append(msg, 0x01)
	msg = binary.BigEndian.AppendUint16(msg, uint16(len(tiles)))
	for _, t := range tiles {
		msg = binary.BigEndian.AppendUint16(msg, uint16(t.r.Min.X))
		msg = binary.BigEndian.AppendUint16(msg, uint16(t.r.Min.Y))
		msg = binary.BigEndian.AppendUint16(msg, uint16(t.r.Dx()))
		msg = binary.BigEndian.AppendUint16(msg, uint16(t.r.Dy()))
		msg = binary.BigEndian.AppendUint32(msg, uint32(len(t.data)))
		msg = append(msg, t.data...)
	}
	return msg
}

// downscale is a simple box filter; good enough to keep text legible at 50–75%.
func downscale(src, dst *image.RGBA, scale float64) *image.RGBA {
	sw, sh := src.Rect.Dx(), src.Rect.Dy()
	dw, dh := max(1, int(float64(sw)*scale)), max(1, int(float64(sh)*scale))
	if dst == nil || dst.Rect.Dx() != dw || dst.Rect.Dy() != dh {
		dst = image.NewRGBA(image.Rect(0, 0, dw, dh))
	}
	for y := 0; y < dh; y++ {
		sy0, sy1 := y*sh/dh, max(y*sh/dh+1, (y+1)*sh/dh)
		for x := 0; x < dw; x++ {
			sx0, sx1 := x*sw/dw, max(x*sw/dw+1, (x+1)*sw/dw)
			var r, g, b, n int
			for yy := sy0; yy < sy1; yy++ {
				o := yy*src.Stride + sx0*4
				for xx := sx0; xx < sx1; xx++ {
					r += int(src.Pix[o])
					g += int(src.Pix[o+1])
					b += int(src.Pix[o+2])
					o += 4
					n++
				}
			}
			d := y*dst.Stride + x*4
			dst.Pix[d], dst.Pix[d+1], dst.Pix[d+2], dst.Pix[d+3] = uint8(r/n), uint8(g/n), uint8(b/n), 255
		}
	}
	return dst
}

// autoScale keeps big screens responsive over a tunnel.
func autoScale(fullW int) float64 {
	switch {
	case fullW > 3000:
		return 0.5
	case fullW > 2000:
		return 0.75
	}
	return 1
}

// ---------- helper process ----------

// msgConn is a message-oriented connection: a WebSocket, or the stdio pipe
// a macOS helper uses to talk to its parent agent.
type msgConn interface {
	Read(ctx context.Context) (websocket.MessageType, []byte, error)
	Write(ctx context.Context, typ websocket.MessageType, p []byte) error
}

// RunDesktopHelper is the entry point of "oceanic-agent desktop-helper <sid>".
// With sid "--stdio" it speaks framed messages on stdin/stdout instead of
// dialing the server (used on macOS, where the helper runs as the GUI user's
// session and the parent daemon owns the network connection).
func RunDesktopHelper(ctx context.Context, sid string) error {
	var conn msgConn
	closeConn := func() {}
	if sid == "--stdio" {
		// The parent closing our stdin means the session is over. Exit right
		// away, even if a frame write to stdout is blocked mid-way.
		conn = newStdioConn(exitOnEOF{os.Stdin}, os.Stdout)
	} else {
		cfg, err := LoadConfig()
		if err != nil {
			return err
		}
		ws, err := dialSession(ctx, newClient(cfg), sid)
		if err != nil {
			return err
		}
		defer ws.CloseNow()
		ws.SetReadLimit(1 << 20)
		conn = ws
		closeConn = func() { ws.Close(websocket.StatusNormalClosure, "") }
	}

	scr, err := openScreen()
	if err != nil {
		sendJSON(ctx, conn, protocol.TermControl{Type: "error", Message: "Can't access the screen: " + err.Error()})
		closeConn()
		return err
	}
	defer scr.Close()
	if n, ok := scr.(interface{ Notice() string }); ok && n.Notice() != "" {
		sendJSON(ctx, conn, protocol.TermControl{Type: "info", Message: n.Notice()})
	}
	err = runDesktopSession(ctx, conn, scr)
	closeConn()
	return err
}

type exitOnEOF struct{ r io.Reader }

func (e exitOnEOF) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil {
		os.Exit(0)
	}
	return n, err
}

// ---------- stdio framing: [type byte][u32 length][payload] ----------

type stdioConn struct {
	r  *bufio.Reader
	mu sync.Mutex
	w  *bufio.Writer
}

func newStdioConn(r io.Reader, w io.Writer) *stdioConn {
	return &stdioConn{r: bufio.NewReaderSize(r, 64<<10), w: bufio.NewWriterSize(w, 256<<10)}
}

func (s *stdioConn) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(s.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > 64<<20 {
		return 0, nil, errors.New("stdio frame too large")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(s.r, buf); err != nil {
		return 0, nil, err
	}
	return websocket.MessageType(hdr[0]), buf, nil
}

func (s *stdioConn) Write(ctx context.Context, typ websocket.MessageType, p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var hdr [5]byte
	hdr[0] = byte(typ)
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(p)))
	if _, err := s.w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := s.w.Write(p); err != nil {
		return err
	}
	return s.w.Flush()
}

// bridgeStdio relays between the server WebSocket and a helper's stdio until
// either side closes.
func bridgeStdio(ctx context.Context, ws *websocket.Conn, helper *stdioConn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		defer cancel()
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil || helper.Write(ctx, typ, data) != nil {
				return
			}
		}
	}()
	for {
		typ, data, err := helper.Read(ctx)
		if err != nil || ws.Write(ctx, typ, data) != nil {
			return
		}
	}
}

// dialSession opens the agent leg of a terminal/desktop session.
func dialSession(ctx context.Context, c *client, sid string) (*websocket.Conn, error) {
	u := "ws" + strings.TrimPrefix(c.cfg.ServerURL, "http") + "/api/agent/session/" + sid
	req, _ := c.newReq(ctx, "GET", "/", nil)
	hdr := req.Header.Clone()
	hdr.Del("Content-Type")
	hc := *c.http
	hc.Timeout = 0
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, u, &websocket.DialOptions{HTTPHeader: hdr, HTTPClient: &hc})
	return conn, err
}

func sendJSON(ctx context.Context, conn msgConn, v any) error {
	b, _ := json.Marshal(v)
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b)
}

func runDesktopSession(parent context.Context, conn msgConn, scr desktopScreen) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	inputs := make(chan desktopInput, 512)
	acks := make(chan struct{}, 16)
	go func() {
		defer cancel()
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				if len(data) == 1 && data[0] == 0x01 {
					select {
					case acks <- struct{}{}:
					default:
					}
				}
				continue
			}
			var ev desktopInput
			if json.Unmarshal(data, &ev) == nil {
				select {
				case inputs <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	enc := &frameEncoder{quality: 60}
	userScale := 0.0 // 0 = auto
	inFlight := 0
	lastW, lastH := 0, 0
	lastFrame := time.Time{}
	timer := time.NewTimer(0)
	defer timer.Stop()
	debug := os.Getenv("OCEANIC_DESKTOP_DEBUG") != ""
	var dbgFrames, dbgBlocked, dbgEmpty int
	var dbgCapture, dbgEncode time.Duration
	dbgAt := time.Now()

	handle := func(ev desktopInput) {
		switch ev.T {
		case "cfg":
			if ev.Q >= 20 && ev.Q <= 95 {
				enc.quality = ev.Q
			}
			if ev.Scale >= 0 && ev.Scale <= 1 {
				userScale = ev.Scale
			}
			enc.prev = nil // full refresh at the new settings
		case "refresh":
			enc.prev = nil
		default:
			if err := scr.Input(ev); err != nil && !errors.Is(err, errUnsupported) {
				log.Printf("desktop input %s: %v", ev.T, err)
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-inputs:
			handle(ev)
			// Drain any burst of input before capturing again.
			for drained := false; !drained; {
				select {
				case ev := <-inputs:
					handle(ev)
				default:
					drained = true
				}
			}
			continue
		case <-acks:
			if inFlight > 0 {
				inFlight--
			}
			continue
		case <-timer.C:
		}

		if debug && time.Since(dbgAt) > 3*time.Second {
			n := max(1, dbgFrames+dbgEmpty)
			log.Printf("desktop: %d frames, %d empty, %d blocked on acks; avg capture %v encode %v",
				dbgFrames, dbgEmpty, dbgBlocked, dbgCapture/time.Duration(n), dbgEncode/time.Duration(n))
			dbgFrames, dbgBlocked, dbgEmpty, dbgCapture, dbgEncode, dbgAt = 0, 0, 0, 0, 0, time.Now()
		}
		if inFlight >= maxInFlight || time.Since(lastFrame) < minFrameGap {
			if inFlight >= maxInFlight {
				dbgBlocked++
			}
			timer.Reset(20 * time.Millisecond)
			continue
		}
		scale := userScale
		if scale == 0 {
			fw, _ := scr.FullSize()
			scale = autoScale(fw)
		}
		tc := time.Now()
		img, err := scr.Capture(scale)
		dbgCapture += time.Since(tc)
		if err != nil {
			// Transient on Windows while the desktop switches (UAC, lock screen).
			timer.Reset(250 * time.Millisecond)
			continue
		}
		w, h := img.Rect.Dx(), img.Rect.Dy()
		if w != lastW || h != lastH {
			lastW, lastH = w, h
			enc.prev = nil
			if err := sendJSON(ctx, conn, map[string]any{"type": "size", "w": w, "h": h}); err != nil {
				return nil
			}
		}
		lastFrame = time.Now()
		te := time.Now()
		msg := enc.encode(img)
		dbgEncode += time.Since(te)
		if msg != nil {
			dbgFrames++
			wctx, wc := context.WithTimeout(ctx, 30*time.Second)
			err := conn.Write(wctx, websocket.MessageBinary, msg)
			wc()
			if err != nil {
				return nil
			}
			inFlight++
			timer.Reset(minFrameGap)
		} else {
			dbgEmpty++
			timer.Reset(idleFrameGap)
		}
	}
}

var errUnsupported = errors.New("unsupported on this platform")

// ---------- keyboard map ----------

// keyDef maps a browser KeyboardEvent.code to a PC/AT set-1 scancode (Windows
// SendInput) and a Linux evdev code (X11 keycode = evdev + 8).
type keyDef struct {
	scan  uint16
	ext   bool
	evdev uint8
}

var keyMap = map[string]keyDef{
	"Escape": {0x01, false, 1}, "Digit1": {0x02, false, 2}, "Digit2": {0x03, false, 3}, "Digit3": {0x04, false, 4},
	"Digit4": {0x05, false, 5}, "Digit5": {0x06, false, 6}, "Digit6": {0x07, false, 7}, "Digit7": {0x08, false, 8},
	"Digit8": {0x09, false, 9}, "Digit9": {0x0A, false, 10}, "Digit0": {0x0B, false, 11}, "Minus": {0x0C, false, 12},
	"Equal": {0x0D, false, 13}, "Backspace": {0x0E, false, 14}, "Tab": {0x0F, false, 15},
	"KeyQ": {0x10, false, 16}, "KeyW": {0x11, false, 17}, "KeyE": {0x12, false, 18}, "KeyR": {0x13, false, 19},
	"KeyT": {0x14, false, 20}, "KeyY": {0x15, false, 21}, "KeyU": {0x16, false, 22}, "KeyI": {0x17, false, 23},
	"KeyO": {0x18, false, 24}, "KeyP": {0x19, false, 25}, "BracketLeft": {0x1A, false, 26}, "BracketRight": {0x1B, false, 27},
	"Enter": {0x1C, false, 28}, "ControlLeft": {0x1D, false, 29},
	"KeyA": {0x1E, false, 30}, "KeyS": {0x1F, false, 31}, "KeyD": {0x20, false, 32}, "KeyF": {0x21, false, 33},
	"KeyG": {0x22, false, 34}, "KeyH": {0x23, false, 35}, "KeyJ": {0x24, false, 36}, "KeyK": {0x25, false, 37},
	"KeyL": {0x26, false, 38}, "Semicolon": {0x27, false, 39}, "Quote": {0x28, false, 40}, "Backquote": {0x29, false, 41},
	"ShiftLeft": {0x2A, false, 42}, "Backslash": {0x2B, false, 43},
	"KeyZ": {0x2C, false, 44}, "KeyX": {0x2D, false, 45}, "KeyC": {0x2E, false, 46}, "KeyV": {0x2F, false, 47},
	"KeyB": {0x30, false, 48}, "KeyN": {0x31, false, 49}, "KeyM": {0x32, false, 50}, "Comma": {0x33, false, 51},
	"Period": {0x34, false, 52}, "Slash": {0x35, false, 53}, "ShiftRight": {0x36, false, 54},
	"NumpadMultiply": {0x37, false, 55}, "AltLeft": {0x38, false, 56}, "Space": {0x39, false, 57}, "CapsLock": {0x3A, false, 58},
	"F1": {0x3B, false, 59}, "F2": {0x3C, false, 60}, "F3": {0x3D, false, 61}, "F4": {0x3E, false, 62}, "F5": {0x3F, false, 63},
	"F6": {0x40, false, 64}, "F7": {0x41, false, 65}, "F8": {0x42, false, 66}, "F9": {0x43, false, 67}, "F10": {0x44, false, 68},
	"NumLock": {0x45, true, 69}, "ScrollLock": {0x46, false, 70},
	"Numpad7": {0x47, false, 71}, "Numpad8": {0x48, false, 72}, "Numpad9": {0x49, false, 73}, "NumpadSubtract": {0x4A, false, 74},
	"Numpad4": {0x4B, false, 75}, "Numpad5": {0x4C, false, 76}, "Numpad6": {0x4D, false, 77}, "NumpadAdd": {0x4E, false, 78},
	"Numpad1": {0x4F, false, 79}, "Numpad2": {0x50, false, 80}, "Numpad3": {0x51, false, 81}, "Numpad0": {0x52, false, 82},
	"NumpadDecimal": {0x53, false, 83}, "IntlBackslash": {0x56, false, 86}, "F11": {0x57, false, 87}, "F12": {0x58, false, 88},
	"NumpadEnter": {0x1C, true, 96}, "ControlRight": {0x1D, true, 97}, "NumpadDivide": {0x35, true, 98},
	"PrintScreen": {0x37, true, 99}, "AltRight": {0x38, true, 100},
	"Home": {0x47, true, 102}, "ArrowUp": {0x48, true, 103}, "PageUp": {0x49, true, 104}, "ArrowLeft": {0x4B, true, 105},
	"ArrowRight": {0x4D, true, 106}, "End": {0x4F, true, 107}, "ArrowDown": {0x50, true, 108}, "PageDown": {0x51, true, 109},
	"Insert": {0x52, true, 110}, "Delete": {0x53, true, 111},
	"MetaLeft": {0x5B, true, 125}, "MetaRight": {0x5C, true, 126}, "ContextMenu": {0x5D, true, 127},
	"OSLeft": {0x5B, true, 125}, "OSRight": {0x5C, true, 126},
}

// runDesktop (in the agent service) starts the helper for a desktop job. If
// the helper can't start, the error is reported straight to the viewer.
func (a *Agent) runDesktop(ctx context.Context, j protocol.Job) {
	log.Printf("desktop %s: starting helper", j.ID)
	dial := func() (*websocket.Conn, error) { return dialSession(ctx, a.c, j.ID) }
	err := launchDesktopHelper(ctx, j.ID, dial, sendSAS)
	if err == nil {
		log.Printf("desktop %s: closed", j.ID)
		return
	}
	log.Printf("desktop %s: %v", j.ID, err)
	if conn, derr := dialSession(ctx, a.c, j.ID); derr == nil {
		sendJSON(ctx, conn, protocol.TermControl{Type: "error", Message: "Remote desktop failed: " + err.Error()})
		conn.Close(websocket.StatusNormalClosure, "")
	}
}
