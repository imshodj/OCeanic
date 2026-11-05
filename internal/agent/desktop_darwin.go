//go:build darwin

package agent

// macOS remote desktop via CoreGraphics, called through purego so the agent
// still cross-compiles from Linux with CGO_ENABLED=0.
//
// macOS requires the user at the Mac to approve, once:
//   - Screen Recording (to see the screen)
//   - Accessibility    (to control mouse and keyboard)
// for "oceanic-agent" in System Settings → Privacy & Security.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"image"
	"log"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/coder/websocket"
	"github.com/ebitengine/purego"
)

type cgPoint struct{ X, Y float64 }
type cgSize struct{ W, H float64 }
type cgRect struct {
	Origin cgPoint
	Size   cgSize
}

var (
	cgOnce sync.Once
	cgErr  error

	cgMainDisplayID          func() uint32
	cgDisplayBounds          func(uint32) cgRect
	cgDisplayCreateImage     func(uint32) uintptr
	cgImageGetWidth          func(uintptr) uintptr
	cgImageGetHeight         func(uintptr) uintptr
	cgImageGetBytesPerRow    func(uintptr) uintptr
	cgImageGetBitsPerPixel   func(uintptr) uintptr
	cgImageGetBitmapInfo     func(uintptr) uint32
	cgImageGetDataProvider   func(uintptr) uintptr
	cgDataProviderCopyData   func(uintptr) uintptr
	cgImageRelease           func(uintptr)
	cgPreflightScreenCapture func() bool
	cgRequestScreenCapture   func() bool
	cgEventCreate            func(uintptr) uintptr
	cgEventGetLocation       func(uintptr) cgPoint
	cgEventCreateMouseEvent  func(uintptr, uint32, cgPoint, uint32) uintptr
	cgEventCreateKeyboard    func(uintptr, uint16, bool) uintptr
	cgEventCreateScroll2     func(uintptr, uint32, uint32, int32, int32, int32) uintptr
	cgEventSetUnicodeString  func(uintptr, uintptr, *uint16)
	cgEventSetFlags          func(uintptr, uint64)
	cgEventSetType           func(uintptr, uint32)
	cgEventSetIntegerField   func(uintptr, uint32, int64)
	cgEventPost              func(uint32, uintptr)
	cfDataGetBytePtr         func(uintptr) *byte
	cfDataGetLength          func(uintptr) int
	cfRelease                func(uintptr)
	cfStringCreateWithCStr   func(uintptr, string, uint32) uintptr
	cfDictionaryCreate       func(uintptr, *uintptr, *uintptr, int, uintptr, uintptr) uintptr
	axIsTrustedWithOptions   func(uintptr) bool

	cfBooleanTrue, cfKeyCallbacks, cfValueCallbacks uintptr
)

func loadCG() error {
	cgOnce.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				cgErr = fmt.Errorf("loading CoreGraphics: %v", r)
			}
		}()
		open := func(p string) uintptr {
			h, err := purego.Dlopen(p, purego.RTLD_NOW|purego.RTLD_GLOBAL)
			if err != nil {
				panic(err)
			}
			return h
		}
		cg := open("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics")
		cf := open("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation")
		as := open("/System/Library/Frameworks/ApplicationServices.framework/ApplicationServices")
		reg := purego.RegisterLibFunc
		reg(&cgMainDisplayID, cg, "CGMainDisplayID")
		reg(&cgDisplayBounds, cg, "CGDisplayBounds")
		reg(&cgDisplayCreateImage, cg, "CGDisplayCreateImage")
		reg(&cgImageGetWidth, cg, "CGImageGetWidth")
		reg(&cgImageGetHeight, cg, "CGImageGetHeight")
		reg(&cgImageGetBytesPerRow, cg, "CGImageGetBytesPerRow")
		reg(&cgImageGetBitsPerPixel, cg, "CGImageGetBitsPerPixel")
		reg(&cgImageGetBitmapInfo, cg, "CGImageGetBitmapInfo")
		reg(&cgImageGetDataProvider, cg, "CGImageGetDataProvider")
		reg(&cgDataProviderCopyData, cg, "CGDataProviderCopyData")
		reg(&cgImageRelease, cg, "CGImageRelease")
		reg(&cgEventCreate, cg, "CGEventCreate")
		reg(&cgEventGetLocation, cg, "CGEventGetLocation")
		reg(&cgEventCreateMouseEvent, cg, "CGEventCreateMouseEvent")
		reg(&cgEventCreateKeyboard, cg, "CGEventCreateKeyboardEvent")
		reg(&cgEventCreateScroll2, cg, "CGEventCreateScrollWheelEvent2")
		reg(&cgEventSetUnicodeString, cg, "CGEventKeyboardSetUnicodeString")
		reg(&cgEventSetFlags, cg, "CGEventSetFlags")
		reg(&cgEventSetType, cg, "CGEventSetType")
		reg(&cgEventSetIntegerField, cg, "CGEventSetIntegerValueField")
		reg(&cgEventPost, cg, "CGEventPost")
		// macOS 10.15+; absent on older systems (no permission model there).
		if _, err := purego.Dlsym(cg, "CGPreflightScreenCaptureAccess"); err == nil {
			reg(&cgPreflightScreenCapture, cg, "CGPreflightScreenCaptureAccess")
			reg(&cgRequestScreenCapture, cg, "CGRequestScreenCaptureAccess")
		}
		reg(&cfDataGetBytePtr, cf, "CFDataGetBytePtr")
		reg(&cfDataGetLength, cf, "CFDataGetLength")
		reg(&cfRelease, cf, "CFRelease")
		reg(&cfStringCreateWithCStr, cf, "CFStringCreateWithCString")
		reg(&cfDictionaryCreate, cf, "CFDictionaryCreate")
		reg(&axIsTrustedWithOptions, as, "AXIsProcessTrustedWithOptions")
		sym := func(h uintptr, n string) uintptr {
			p, err := purego.Dlsym(h, n)
			if err != nil {
				panic(err)
			}
			return p
		}
		cfBooleanTrue = *(*uintptr)(unsafe.Add(nil, sym(cf, "kCFBooleanTrue"))) // C global, not Go memory
		cfKeyCallbacks = sym(cf, "kCFTypeDictionaryKeyCallBacks")
		cfValueCallbacks = sym(cf, "kCFTypeDictionaryValueCallBacks")
	})
	return cgErr
}

// accessibilityTrusted checks (and, with prompt, asks for) Accessibility access.
func accessibilityTrusted(prompt bool) bool {
	if !prompt {
		return axIsTrustedWithOptions(0)
	}
	key := cfStringCreateWithCStr(0, "AXTrustedCheckOptionPrompt\x00", 0x08000100) // kCFStringEncodingUTF8
	val := cfBooleanTrue
	dict := cfDictionaryCreate(0, &key, &val, 1, cfKeyCallbacks, cfValueCallbacks)
	ok := axIsTrustedWithOptions(dict)
	cfRelease(dict)
	cfRelease(key)
	return ok
}

const (
	evLeftDown, evLeftUp, evRightDown, evRightUp = 1, 2, 3, 4
	evMoved, evLeftDragged, evRightDragged       = 5, 6, 7
	evOtherDown, evOtherUp, evOtherDragged       = 25, 26, 27
	evFlagsChanged                               = 12
	fieldClickState                              = 1
	tapHID                                       = 0

	flagShift   = 0x00020000
	flagControl = 0x00040000
	flagOption  = 0x00080000
	flagCommand = 0x00100000
)

type macScreen struct {
	display  uint32
	img      *image.RGBA
	scaled   *image.RGBA
	pxW, pxH int
	canInput bool

	held      map[int]bool // mouse buttons down
	flags     uint64
	lastClick time.Time
	lastBtn   int
	clicks    int64
}

func openScreen() (desktopScreen, error) {
	if err := loadCG(); err != nil {
		return nil, err
	}
	if cgPreflightScreenCapture != nil && !cgPreflightScreenCapture() {
		cgRequestScreenCapture() // shows the system prompt on the Mac
		return nil, errors.New("macOS needs Screen Recording permission. On the Mac, open System Settings → Privacy & Security → Screen Recording, " +
			"turn on \"oceanic-agent\" (use + to add /usr/local/oceanic/oceanic-agent if it isn't listed), then reconnect")
	}
	s := &macScreen{display: cgMainDisplayID(), held: map[int]bool{}}
	s.canInput = accessibilityTrusted(true)
	if _, err := s.Capture(1); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *macScreen) Notice() string {
	if s.canInput {
		return ""
	}
	return "View only: to control this Mac, allow \"oceanic-agent\" in System Settings → Privacy & Security → Accessibility, then reconnect."
}

func (s *macScreen) FullSize() (int, int) { return s.pxW, s.pxH }

func (s *macScreen) Capture(scale float64) (*image.RGBA, error) {
	s.display = cgMainDisplayID()
	ref := cgDisplayCreateImage(s.display)
	if ref == 0 {
		return nil, errors.New("CGDisplayCreateImage returned nothing (screen locked or permission missing?)")
	}
	defer cgImageRelease(ref)
	w, h := int(cgImageGetWidth(ref)), int(cgImageGetHeight(ref))
	bpr := int(cgImageGetBytesPerRow(ref))
	if cgImageGetBitsPerPixel(ref) != 32 {
		return nil, errors.New("unsupported pixel format")
	}
	info := cgImageGetBitmapInfo(ref)
	data := cgDataProviderCopyData(cgImageGetDataProvider(ref))
	if data == 0 {
		return nil, errors.New("no pixel data")
	}
	defer cfRelease(data)
	src := unsafe.Slice(cfDataGetBytePtr(data), cfDataGetLength(data))

	if s.img == nil || s.img.Rect.Dx() != w || s.img.Rect.Dy() != h {
		s.img = image.NewRGBA(image.Rect(0, 0, w, h))
	}
	s.pxW, s.pxH = w, h
	// Channel order from the bitmap info: little-endian + alpha-first is BGRA
	// (the usual case); big-endian alpha-first is ARGB; otherwise RGBA.
	little := info&0x7000 == 0x2000
	alpha := info & 0x1F
	alphaFirst := alpha == 1 || alpha == 2 || alpha == 4 || alpha == 6 // premultiplied/plain/none-skip first
	ri, gi, bi := 0, 1, 2
	switch {
	case little && alphaFirst:
		ri, gi, bi = 2, 1, 0
	case !little && alphaFirst:
		ri, gi, bi = 1, 2, 3
	case little && !alphaFirst:
		ri, gi, bi = 3, 2, 1
	}
	dst := s.img.Pix
	for y := 0; y < h; y++ {
		row := src[y*bpr : y*bpr+w*4]
		d := dst[y*s.img.Stride:]
		for i := 0; i < w*4; i += 4 {
			d[i], d[i+1], d[i+2], d[i+3] = row[i+ri], row[i+gi], row[i+bi], 255
		}
	}
	s.drawCursor()
	if scale >= 0.999 {
		return s.img, nil
	}
	s.scaled = downscale(s.img, s.scaled, scale)
	return s.scaled, nil
}

// cursorShape is a classic arrow; '#' = black outline, '.' = white fill.
var cursorShape = []string{
	"#", "##", "#.#", "#..#", "#...#", "#....#", "#.....#", "#......#", "#.......#",
	"#........#", "#.....#####", "#..#..#", "#.# #..#", "##  #..#", "#    #..#", "     #..#", "      ##",
}

// drawCursor paints the pointer, since macOS screen grabs omit it.
func (s *macScreen) drawCursor() {
	ev := cgEventCreate(0)
	if ev == 0 {
		return
	}
	loc := cgEventGetLocation(ev)
	cfRelease(ev)
	b := cgDisplayBounds(s.display)
	if b.Size.W <= 0 {
		return
	}
	k := float64(s.pxW) / b.Size.W // backing scale (2 on Retina)
	ox, oy := int((loc.X-b.Origin.X)*k), int((loc.Y-b.Origin.Y)*k)
	px := max(1, int(k+0.5))
	for cy, line := range cursorShape {
		for cx, ch := range line {
			var c byte
			switch ch {
			case '#':
				c = 0
			case '.':
				c = 255
			default:
				continue
			}
			for dy := 0; dy < px; dy++ {
				for dx := 0; dx < px; dx++ {
					x, y := ox+cx*px+dx, oy+cy*px+dy
					if x < 0 || y < 0 || x >= s.pxW || y >= s.pxH {
						continue
					}
					o := y*s.img.Stride + x*4
					s.img.Pix[o], s.img.Pix[o+1], s.img.Pix[o+2] = c, c, c
				}
			}
		}
	}
}

func (s *macScreen) point(ev desktopInput) cgPoint {
	b := cgDisplayBounds(s.display)
	return cgPoint{b.Origin.X + clampF(ev.X)*(b.Size.W-1), b.Origin.Y + clampF(ev.Y)*(b.Size.H-1)}
}

func (s *macScreen) post(ev uintptr) {
	if ev == 0 {
		return
	}
	cgEventSetFlags(ev, s.flags)
	cgEventPost(tapHID, ev)
	cfRelease(ev)
}

var macModifierFlags = map[string]uint64{
	"ShiftLeft": flagShift, "ShiftRight": flagShift, "ControlLeft": flagControl, "ControlRight": flagControl,
	"AltLeft": flagOption, "AltRight": flagOption, "MetaLeft": flagCommand, "MetaRight": flagCommand,
	"OSLeft": flagCommand, "OSRight": flagCommand,
}

func (s *macScreen) Input(ev desktopInput) error {
	if !s.canInput {
		return nil // view only until Accessibility is granted
	}
	switch ev.T {
	case "mm":
		typ, btn := uint32(evMoved), uint32(0)
		switch {
		case s.held[0]:
			typ = evLeftDragged
		case s.held[2]:
			typ, btn = evRightDragged, 1
		case s.held[1]:
			typ, btn = evOtherDragged, 2
		}
		s.post(cgEventCreateMouseEvent(0, typ, s.point(ev), btn))
	case "md", "mu":
		down := ev.T == "md"
		var typ, btn uint32
		switch ev.B {
		case 0:
			typ, btn = map[bool]uint32{true: evLeftDown, false: evLeftUp}[down], 0
		case 2:
			typ, btn = map[bool]uint32{true: evRightDown, false: evRightUp}[down], 1
		default:
			typ, btn = map[bool]uint32{true: evOtherDown, false: evOtherUp}[down], 2
		}
		if down {
			// macOS needs the click count on the event for double/triple clicks.
			if ev.B == s.lastBtn && time.Since(s.lastClick) < 450*time.Millisecond {
				s.clicks++
			} else {
				s.clicks = 1
			}
			s.lastClick, s.lastBtn = time.Now(), ev.B
		}
		s.held[ev.B] = down
		e := cgEventCreateMouseEvent(0, typ, s.point(ev), btn)
		if e != 0 {
			cgEventSetIntegerField(e, fieldClickState, s.clicks)
		}
		s.post(e)
	case "wh":
		// Positive dy from the browser means "scroll down" = negative wheel delta on macOS.
		if ev.DY != 0 || ev.DX != 0 {
			s.post(cgEventCreateScroll2(0, 1 /* kCGScrollEventUnitLine */, 2, int32(-ev.DY*3), int32(-ev.DX*3), 0))
		}
	case "kd", "ku":
		down := ev.T == "kd"
		if f, ok := macModifierFlags[ev.Code]; ok {
			if down {
				s.flags |= f
			} else {
				s.flags &^= f
			}
			e := cgEventCreateKeyboard(0, macKeys[ev.Code], down)
			if e != 0 {
				cgEventSetType(e, evFlagsChanged)
			}
			s.post(e)
			return nil
		}
		code, ok := macKeys[ev.Code]
		if !ok {
			return nil
		}
		s.post(cgEventCreateKeyboard(0, code, down))
	case "text":
		saved := s.flags
		s.flags = 0
		for _, r := range ev.S {
			if r == '\r' {
				continue
			}
			if r == '\n' {
				s.post(cgEventCreateKeyboard(0, 0x24, true))
				s.post(cgEventCreateKeyboard(0, 0x24, false))
				continue
			}
			u := utf16.Encode([]rune{r})
			for _, down := range []bool{true, false} {
				e := cgEventCreateKeyboard(0, 0, down)
				if e != 0 {
					cgEventSetUnicodeString(e, uintptr(len(u)), &u[0])
				}
				s.post(e)
			}
		}
		s.flags = saved
	case "cad":
		return errUnsupported
	}
	return nil
}

func (s *macScreen) Close() {
	// Release anything still held so the Mac isn't left with a stuck button.
	for b, down := range s.held {
		if down {
			s.Input(desktopInput{T: "mu", B: b, X: 0.5, Y: 0.5})
		}
	}
}

func clampF(f float64) float64 { return max(0, min(1, f)) }

// macKeys maps KeyboardEvent.code to macOS virtual key codes (kVK_*).
var macKeys = map[string]uint16{
	"KeyA": 0x00, "KeyS": 0x01, "KeyD": 0x02, "KeyF": 0x03, "KeyH": 0x04, "KeyG": 0x05, "KeyZ": 0x06, "KeyX": 0x07,
	"KeyC": 0x08, "KeyV": 0x09, "IntlBackslash": 0x0A, "KeyB": 0x0B, "KeyQ": 0x0C, "KeyW": 0x0D, "KeyE": 0x0E, "KeyR": 0x0F,
	"KeyY": 0x10, "KeyT": 0x11, "Digit1": 0x12, "Digit2": 0x13, "Digit3": 0x14, "Digit4": 0x15, "Digit6": 0x16, "Digit5": 0x17,
	"Equal": 0x18, "Digit9": 0x19, "Digit7": 0x1A, "Minus": 0x1B, "Digit8": 0x1C, "Digit0": 0x1D, "BracketRight": 0x1E,
	"KeyO": 0x1F, "KeyU": 0x20, "BracketLeft": 0x21, "KeyI": 0x22, "KeyP": 0x23, "Enter": 0x24, "KeyL": 0x25, "KeyJ": 0x26,
	"Quote": 0x27, "KeyK": 0x28, "Semicolon": 0x29, "Backslash": 0x2A, "Comma": 0x2B, "Slash": 0x2C, "KeyN": 0x2D,
	"KeyM": 0x2E, "Period": 0x2F, "Tab": 0x30, "Space": 0x31, "Backquote": 0x32, "Backspace": 0x33, "Escape": 0x35,
	"MetaRight": 0x36, "OSRight": 0x36, "MetaLeft": 0x37, "OSLeft": 0x37, "ShiftLeft": 0x38, "CapsLock": 0x39,
	"AltLeft": 0x3A, "ControlLeft": 0x3B, "ShiftRight": 0x3C, "AltRight": 0x3D, "ControlRight": 0x3E,
	"NumpadDecimal": 0x41, "NumpadMultiply": 0x43, "NumpadAdd": 0x45, "NumLock": 0x47, "NumpadDivide": 0x4B,
	"NumpadEnter": 0x4C, "NumpadSubtract": 0x4E, "NumpadEqual": 0x51, "Numpad0": 0x52, "Numpad1": 0x53, "Numpad2": 0x54,
	"Numpad3": 0x55, "Numpad4": 0x56, "Numpad5": 0x57, "Numpad6": 0x58, "Numpad7": 0x59, "Numpad8": 0x5B, "Numpad9": 0x5C,
	"F5": 0x60, "F6": 0x61, "F7": 0x62, "F3": 0x63, "F8": 0x64, "F9": 0x65, "F11": 0x67, "PrintScreen": 0x69,
	"ScrollLock": 0x6B, "F10": 0x6D, "F12": 0x6F, "Pause": 0x71, "Insert": 0x72, "Home": 0x73, "PageUp": 0x74,
	"Delete": 0x75, "F4": 0x76, "End": 0x77, "F2": 0x78, "PageDown": 0x79, "F1": 0x7A, "ArrowLeft": 0x7B,
	"ArrowRight": 0x7C, "ArrowDown": 0x7D, "ArrowUp": 0x7E,
}

// ---------- daemon side: run the helper in the logged-in user's GUI session ----------

func consoleUID() (uint32, error) {
	st, err := os.Stat("/dev/console")
	if err != nil {
		return 0, err
	}
	uid := st.Sys().(*syscall.Stat_t).Uid
	if uid == 0 {
		return 0, errors.New("nobody is logged in on this Mac; sharing the login window isn't supported yet, so log in first")
	}
	return uid, nil
}

func launchDesktopHelper(ctx context.Context, sid string, dial func() (*websocket.Conn, error), onSAS func()) error {
	uid, err := consoleUID()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	exe, _ := os.Executable()
	// "launchctl asuser" runs the helper in the user's GUI bootstrap
	// namespace (so it can reach the WindowServer); it stays root, and talks
	// to us over stdio so it never needs the agent's credentials.
	cmd := exec.CommandContext(ctx, "launchctl", "asuser", strconv.Itoa(int(uid)), exe, "desktop-helper", "--stdio")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launchctl asuser: %w", err)
	}
	log.Printf("desktop %s: helper pid %d for uid %d", sid, cmd.Process.Pid, uid)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			log.Printf("desktop-helper: %s", sc.Text())
		}
	}()
	ws, err := dial()
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return err
	}
	ws.SetReadLimit(1 << 20)
	bridgeStdio(ctx, ws, newStdioConn(stdout, stdin))
	ws.Close(websocket.StatusNormalClosure, "")
	stdin.Close()
	cancel()
	cmd.Wait()
	return nil
}

func sendSAS() {}
