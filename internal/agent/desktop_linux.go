//go:build linux

package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/coder/websocket"
	"image"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
)

// ---------- X11 screen ----------

type x11Screen struct {
	c      *xgb.Conn
	root   xproto.Window
	w, h   int
	full   *image.RGBA
	scaled *image.RGBA
}

func openScreen() (desktopScreen, error) {
	if os.Getenv("WAYLAND_DISPLAY") != "" && os.Getenv("DISPLAY") == "" {
		return nil, errors.New("Wayland sessions aren't supported; log in with an X11 (Xorg) session")
	}
	c, err := xgb.NewConn()
	if err != nil {
		return nil, fmt.Errorf("connect to X display %q: %w", os.Getenv("DISPLAY"), err)
	}
	if err := xtest.Init(c); err != nil {
		c.Close()
		return nil, fmt.Errorf("XTEST extension unavailable: %w", err)
	}
	scr := xproto.Setup(c).DefaultScreen(c)
	return &x11Screen{c: c, root: scr.Root, w: int(scr.WidthInPixels), h: int(scr.HeightInPixels)}, nil
}

func (s *x11Screen) FullSize() (int, int) { return s.w, s.h }

func (s *x11Screen) Capture(scale float64) (*image.RGBA, error) {
	// Track resolution changes (xrandr).
	if g, err := xproto.GetGeometry(s.c, xproto.Drawable(s.root)).Reply(); err == nil {
		s.w, s.h = int(g.Width), int(g.Height)
	}
	if s.full == nil || s.full.Rect.Dx() != s.w || s.full.Rect.Dy() != s.h {
		s.full = image.NewRGBA(image.Rect(0, 0, s.w, s.h))
	}
	// Fetch in bands to keep individual replies modest.
	const band = 256
	for y := 0; y < s.h; y += band {
		bh := min(band, s.h-y)
		rep, err := xproto.GetImage(s.c, xproto.ImageFormatZPixmap, xproto.Drawable(s.root), 0, int16(y), uint16(s.w), uint16(bh), 0xffffffff).Reply()
		if err != nil {
			return nil, err
		}
		if len(rep.Data) < s.w*bh*4 {
			return nil, fmt.Errorf("unsupported X visual (depth %d)", rep.Depth)
		}
		// 24/32-bit TrueColor, LSB first: B G R X  ->  R G B A
		dst := s.full.Pix[y*s.full.Stride:]
		src := rep.Data
		for i := 0; i+3 < s.w*bh*4; i += 4 {
			dst[i], dst[i+1], dst[i+2], dst[i+3] = src[i+2], src[i+1], src[i], 255
		}
	}
	if scale >= 0.999 {
		return s.full, nil
	}
	s.scaled = downscale(s.full, s.scaled, scale)
	return s.scaled, nil
}

func (s *x11Screen) fake(typ byte, detail byte, x, y int16) error {
	return xtest.FakeInputChecked(s.c, typ, detail, 0, s.root, x, y, 0).Check()
}

func (s *x11Screen) Input(ev desktopInput) error {
	px := func() (int16, int16) {
		return int16(clampF(ev.X) * float64(s.w-1)), int16(clampF(ev.Y) * float64(s.h-1))
	}
	switch ev.T {
	case "mm":
		x, y := px()
		return s.fake(xproto.MotionNotify, 0, x, y)
	case "md", "mu":
		btn := map[int]byte{0: 1, 1: 2, 2: 3}[ev.B]
		if btn == 0 {
			return nil
		}
		x, y := px()
		s.fake(xproto.MotionNotify, 0, x, y)
		t := byte(xproto.ButtonPress)
		if ev.T == "mu" {
			t = xproto.ButtonRelease
		}
		return s.fake(t, btn, 0, 0)
	case "wh":
		var btn byte
		n := ev.DY
		switch {
		case ev.DY < 0:
			btn, n = 4, -ev.DY
		case ev.DY > 0:
			btn = 5
		case ev.DX < 0:
			btn, n = 6, -ev.DX
		case ev.DX > 0:
			btn, n = 7, ev.DX
		}
		for i := 0; i < min(n, 10) && btn != 0; i++ {
			s.fake(xproto.ButtonPress, btn, 0, 0)
			s.fake(xproto.ButtonRelease, btn, 0, 0)
		}
		return nil
	case "kd", "ku":
		k, ok := keyMap[ev.Code]
		if !ok {
			return nil
		}
		t := byte(xproto.KeyPress)
		if ev.T == "ku" {
			t = xproto.KeyRelease
		}
		return s.fake(t, k.evdev+8, 0, 0)
	case "text":
		for _, r := range ev.S {
			code, shift, ok := asciiKey(r)
			if !ok {
				continue
			}
			if shift {
				s.fake(xproto.KeyPress, keyMap["ShiftLeft"].evdev+8, 0, 0)
			}
			k := keyMap[code].evdev + 8
			s.fake(xproto.KeyPress, k, 0, 0)
			s.fake(xproto.KeyRelease, k, 0, 0)
			if shift {
				s.fake(xproto.KeyRelease, keyMap["ShiftLeft"].evdev+8, 0, 0)
			}
		}
		return nil
	case "cad":
		return errUnsupported
	}
	return nil
}

func (s *x11Screen) Close() { s.c.Close() }

func clampF(f float64) float64 { return max(0, min(1, f)) }

// asciiKey maps a character to a US-layout key (X11 text paste).
func asciiKey(r rune) (code string, shift bool, ok bool) {
	switch {
	case r >= 'a' && r <= 'z':
		return "Key" + strings.ToUpper(string(r)), false, true
	case r >= 'A' && r <= 'Z':
		return "Key" + string(r), true, true
	case r >= '0' && r <= '9':
		return "Digit" + string(r), false, true
	}
	if c, ok := usPunct[r]; ok {
		return c.code, c.shift, true
	}
	return "", false, false
}

var usPunct = map[rune]struct {
	code  string
	shift bool
}{
	' ': {"Space", false}, '\n': {"Enter", false}, '\t': {"Tab", false},
	'-': {"Minus", false}, '_': {"Minus", true}, '=': {"Equal", false}, '+': {"Equal", true},
	'[': {"BracketLeft", false}, '{': {"BracketLeft", true}, ']': {"BracketRight", false}, '}': {"BracketRight", true},
	';': {"Semicolon", false}, ':': {"Semicolon", true}, '\'': {"Quote", false}, '"': {"Quote", true},
	'`': {"Backquote", false}, '~': {"Backquote", true}, '\\': {"Backslash", false}, '|': {"Backslash", true},
	',': {"Comma", false}, '<': {"Comma", true}, '.': {"Period", false}, '>': {"Period", true},
	'/': {"Slash", false}, '?': {"Slash", true}, '!': {"Digit1", true}, '@': {"Digit2", true}, '#': {"Digit3", true},
	'$': {"Digit4", true}, '%': {"Digit5", true}, '^': {"Digit6", true}, '&': {"Digit7", true}, '*': {"Digit8", true},
	'(': {"Digit9", true}, ')': {"Digit0", true},
}

// ---------- launching the helper on the active X display ----------

// findX11 locates a running X display and its auth cookie by looking at
// other processes' environments (we run as root, so we can read them).
func findX11() (display, xauth string) {
	if d := os.Getenv("OCEANIC_DISPLAY"); d != "" {
		return d, os.Getenv("OCEANIC_XAUTHORITY")
	}
	procs, _ := filepath.Glob("/proc/[0-9]*/environ")
	for _, p := range procs {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var d, a string
		for _, kv := range bytes.Split(b, []byte{0}) {
			if v, ok := bytes.CutPrefix(kv, []byte("DISPLAY=")); ok {
				d = string(v)
			} else if v, ok := bytes.CutPrefix(kv, []byte("XAUTHORITY=")); ok {
				a = string(v)
			}
		}
		if strings.HasPrefix(d, ":") {
			return d, a
		}
	}
	// Fall back to an Xorg/Xvfb command line ("-auth /path" and ":N").
	cmds, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	for _, p := range cmds {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		args := strings.Split(string(b), "\x00")
		if len(args) == 0 || !strings.Contains(filepath.Base(args[0]), "X") {
			continue
		}
		var d, a string
		for i, arg := range args {
			if strings.HasPrefix(arg, ":") {
				if _, err := strconv.Atoi(arg[1:]); err == nil {
					d = arg
				}
			}
			if arg == "-auth" && i+1 < len(args) {
				a = args[i+1]
			}
		}
		if d != "" {
			return d, a
		}
	}
	if _, err := os.Stat("/tmp/.X11-unix/X0"); err == nil {
		return ":0", ""
	}
	return "", ""
}

// launchDesktopHelper runs "desktop-helper" against the active X display and
// waits for it to finish.
func launchDesktopHelper(ctx context.Context, sid string, dial func() (*websocket.Conn, error), onSAS func()) error {
	display, xauth := findX11()
	if display == "" {
		return errors.New("no graphical (X11) session found on this machine")
	}
	exe, _ := os.Executable()
	cmd := exec.CommandContext(ctx, exe, "desktop-helper", sid)
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	if xauth != "" {
		cmd.Env = append(cmd.Env, "XAUTHORITY="+xauth)
	}
	if p := os.Getenv("OCEANIC_AGENT_CONFIG"); p != "" {
		cmd.Env = append(cmd.Env, "OCEANIC_AGENT_CONFIG="+p)
	}
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	log.Printf("desktop %s: helper pid %d on display %s", sid, cmd.Process.Pid, display)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			log.Printf("desktop-helper: %s", sc.Text())
		}
	}()
	return cmd.Wait()
}

func sendSAS() {}
