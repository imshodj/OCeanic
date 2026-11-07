//go:build windows

package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/coder/websocket"
	"image"
	"log"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	user32 = windows.NewLazySystemDLL("user32.dll")
	gdi32  = windows.NewLazySystemDLL("gdi32.dll")
	sasDLL = windows.NewLazySystemDLL("sas.dll")

	pGetDC                 = user32.NewProc("GetDC")
	pReleaseDC             = user32.NewProc("ReleaseDC")
	pGetSystemMetrics      = user32.NewProc("GetSystemMetrics")
	pOpenInputDesktop      = user32.NewProc("OpenInputDesktop")
	pSetThreadDesktop      = user32.NewProc("SetThreadDesktop")
	pCloseDesktop          = user32.NewProc("CloseDesktop")
	pGetUserObjectInfo     = user32.NewProc("GetUserObjectInformationW")
	pOpenWindowStation     = user32.NewProc("OpenWindowStationW")
	pSetProcessWindowSt    = user32.NewProc("SetProcessWindowStation")
	pGetCursorInfo         = user32.NewProc("GetCursorInfo")
	pGetIconInfo           = user32.NewProc("GetIconInfo")
	pDrawIconEx            = user32.NewProc("DrawIconEx")
	pSendInput             = user32.NewProc("SendInput")
	pMapVirtualKey         = user32.NewProc("MapVirtualKeyW")
	pSetProcessDPIAware    = user32.NewProc("SetProcessDPIAware")
	pSetProcessDpiAwareCtx = user32.NewProc("SetProcessDpiAwarenessContext")
	pCreateCompatibleDC    = gdi32.NewProc("CreateCompatibleDC")
	pCreateDIBSection      = gdi32.NewProc("CreateDIBSection")
	pSelectObject          = gdi32.NewProc("SelectObject")
	pDeleteObject          = gdi32.NewProc("DeleteObject")
	pDeleteDC              = gdi32.NewProc("DeleteDC")
	pBitBlt                = gdi32.NewProc("BitBlt")
	pStretchBlt            = gdi32.NewProc("StretchBlt")
	pSetStretchBltMode     = gdi32.NewProc("SetStretchBltMode")
	pSetBrushOrgEx         = gdi32.NewProc("SetBrushOrgEx")
	pSendSAS               = sasDLL.NewProc("SendSAS")
)

const (
	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79
	srcCopy           = 0x00CC0020
	captureBlt        = 0x40000000
	halftone          = 4
	diNormal          = 3
	cursorShowing     = 1
	genericAll        = 0x10000000
	uoiName           = 2

	inputMouse    = 0
	inputKeyboard = 1

	mouseMove        = 0x0001
	mouseLeftDown    = 0x0002
	mouseLeftUp      = 0x0004
	mouseRightDown   = 0x0008
	mouseRightUp     = 0x0010
	mouseMiddleDown  = 0x0020
	mouseMiddleUp    = 0x0040
	mouseWheel       = 0x0800
	mouseHWheel      = 0x1000
	mouseVirtualDesk = 0x4000
	mouseAbsolute    = 0x8000

	keyExtended = 0x0001
	keyUp       = 0x0002
	keyUnicode  = 0x0004
	keyScancode = 0x0008
)

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type cursorInfo struct {
	Size   uint32
	Flags  uint32
	Cursor windows.Handle
	X, Y   int32
}

type iconInfo struct {
	Icon     int32
	XHotspot uint32
	YHotspot uint32
	Mask     windows.Handle
	Color    windows.Handle
}

// INPUT is 40 bytes on 64-bit Windows (4-byte type + padding + 32-byte union).
type mouseInputT struct {
	typ   uint32
	_     uint32
	dx    int32
	dy    int32
	data  uint32
	flags uint32
	time  uint32
	extra uintptr
}

type keybdInputT struct {
	typ   uint32
	_     uint32
	vk    uint16
	scan  uint16
	flags uint32
	time  uint32
	extra uintptr
	_     [8]byte
}

type gdiScreen struct {
	desk        uintptr
	deskName    string
	memDC       uintptr
	bmp, oldBmp uintptr
	bits        unsafe.Pointer
	bw, bh      int
	vx, vy      int
	vw, vh      int
	img         *image.RGBA
}

func openScreen() (desktopScreen, error) {
	// Physical pixels, not DPI-scaled ones.
	if pSetProcessDpiAwareCtx.Find() == nil {
		pSetProcessDpiAwareCtx.Call(^uintptr(3)) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4)
	} else {
		pSetProcessDPIAware.Call()
	}
	// We were started in the user's session but in a non-interactive window
	// station; attach to the interactive one.
	name, _ := windows.UTF16PtrFromString("WinSta0")
	ws, _, err := pOpenWindowStation.Call(uintptr(unsafe.Pointer(name)), 0, windows.MAXIMUM_ALLOWED)
	if ws == 0 {
		return nil, fmt.Errorf("OpenWindowStation: %v", err)
	}
	pSetProcessWindowSt.Call(ws)
	// The input desktop is attached lazily by Capture/Input, which run on the
	// session's OS-locked thread (SetThreadDesktop is per-thread).
	return &gdiScreen{}, nil
}

// syncDesktop follows the input desktop (Default <-> Winlogon/UAC secure desktop).
func (s *gdiScreen) syncDesktop() error {
	d, _, err := pOpenInputDesktop.Call(0, 0, genericAll)
	if d == 0 {
		return fmt.Errorf("OpenInputDesktop: %v", err)
	}
	var buf [128]uint16
	var n uint32
	pGetUserObjectInfo.Call(d, uoiName, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)*2), uintptr(unsafe.Pointer(&n)))
	name := windows.UTF16ToString(buf[:])
	if name == s.deskName && s.desk != 0 {
		pCloseDesktop.Call(d)
		return nil
	}
	if r, _, err := pSetThreadDesktop.Call(d); r == 0 {
		pCloseDesktop.Call(d)
		return fmt.Errorf("SetThreadDesktop(%s): %v", name, err)
	}
	if s.desk != 0 {
		pCloseDesktop.Call(s.desk)
	}
	s.desk, s.deskName = d, name
	s.freeBitmap() // DCs are per-desktop
	return nil
}

func metric(i int) int {
	r, _, _ := pGetSystemMetrics.Call(uintptr(i))
	return int(int32(r))
}

func (s *gdiScreen) FullSize() (int, int) {
	return metric(smCXVirtualScreen), metric(smCYVirtualScreen)
}

func (s *gdiScreen) freeBitmap() {
	if s.memDC != 0 {
		pSelectObject.Call(s.memDC, s.oldBmp)
		pDeleteObject.Call(s.bmp)
		pDeleteDC.Call(s.memDC)
		s.memDC, s.bmp, s.bits = 0, 0, nil
	}
}

func (s *gdiScreen) ensureBitmap(screenDC uintptr, w, h int) error {
	if s.memDC != 0 && s.bw == w && s.bh == h {
		return nil
	}
	s.freeBitmap()
	dc, _, _ := pCreateCompatibleDC.Call(screenDC)
	if dc == 0 {
		return errors.New("CreateCompatibleDC failed")
	}
	bi := bitmapInfoHeader{Size: uint32(unsafe.Sizeof(bitmapInfoHeader{})), Width: int32(w), Height: -int32(h), Planes: 1, BitCount: 32}
	var bits unsafe.Pointer
	bmp, _, _ := pCreateDIBSection.Call(dc, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if bmp == 0 {
		pDeleteDC.Call(dc)
		return errors.New("CreateDIBSection failed")
	}
	old, _, _ := pSelectObject.Call(dc, bmp)
	s.memDC, s.bmp, s.oldBmp, s.bits, s.bw, s.bh = dc, bmp, old, bits, w, h
	return nil
}

func (s *gdiScreen) Capture(scale float64) (*image.RGBA, error) {
	if err := s.syncDesktop(); err != nil {
		return nil, err
	}
	s.vx, s.vy = metric(smXVirtualScreen), metric(smYVirtualScreen)
	s.vw, s.vh = metric(smCXVirtualScreen), metric(smCYVirtualScreen)
	if s.vw <= 0 || s.vh <= 0 {
		return nil, errors.New("no screen")
	}
	w, h := s.vw, s.vh
	if scale < 0.999 {
		w, h = max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))
	}
	sdc, _, _ := pGetDC.Call(0)
	if sdc == 0 {
		return nil, errors.New("GetDC failed")
	}
	defer pReleaseDC.Call(0, sdc)
	if err := s.ensureBitmap(sdc, w, h); err != nil {
		return nil, err
	}
	var ok uintptr
	if w == s.vw && h == s.vh {
		ok, _, _ = pBitBlt.Call(s.memDC, 0, 0, uintptr(w), uintptr(h), sdc, uintptr(s.vx), uintptr(s.vy), srcCopy|captureBlt)
	} else {
		pSetStretchBltMode.Call(s.memDC, halftone)
		pSetBrushOrgEx.Call(s.memDC, 0, 0, 0)
		ok, _, _ = pStretchBlt.Call(s.memDC, 0, 0, uintptr(w), uintptr(h), sdc, uintptr(s.vx), uintptr(s.vy), uintptr(s.vw), uintptr(s.vh), srcCopy|captureBlt)
	}
	if ok == 0 {
		return nil, errors.New("BitBlt failed (desktop switching?)")
	}
	s.drawCursor(float64(w) / float64(s.vw))

	if s.img == nil || s.img.Rect.Dx() != w || s.img.Rect.Dy() != h {
		s.img = image.NewRGBA(image.Rect(0, 0, w, h))
	}
	src := unsafe.Slice((*byte)(s.bits), w*h*4)
	dst := s.img.Pix
	for i := 0; i < len(src); i += 4 { // BGRA -> RGBA
		dst[i], dst[i+1], dst[i+2], dst[i+3] = src[i+2], src[i+1], src[i], 255
	}
	return s.img, nil
}

func (s *gdiScreen) drawCursor(scale float64) {
	ci := cursorInfo{Size: uint32(unsafe.Sizeof(cursorInfo{}))}
	if r, _, _ := pGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci))); r == 0 || ci.Flags&cursorShowing == 0 {
		return
	}
	var ii iconInfo
	hx, hy := int32(0), int32(0)
	if r, _, _ := pGetIconInfo.Call(uintptr(ci.Cursor), uintptr(unsafe.Pointer(&ii))); r != 0 {
		hx, hy = int32(ii.XHotspot), int32(ii.YHotspot)
		if ii.Mask != 0 {
			pDeleteObject.Call(uintptr(ii.Mask))
		}
		if ii.Color != 0 {
			pDeleteObject.Call(uintptr(ii.Color))
		}
	}
	x := int32(float64(ci.X-int32(s.vx)-hx) * scale)
	y := int32(float64(ci.Y-int32(s.vy)-hy) * scale)
	pDrawIconEx.Call(s.memDC, uintptr(x), uintptr(y), uintptr(ci.Cursor), 0, 0, 0, 0, diNormal)
}

func sendMouse(dx, dy int32, data, flags uint32) error {
	in := mouseInputT{typ: inputMouse, dx: dx, dy: dy, data: data, flags: flags}
	if r, _, err := pSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in)); r == 0 {
		return fmt.Errorf("SendInput: %v", err)
	}
	return nil
}

func sendKey(scan uint16, flags uint32) error {
	in := keybdInputT{typ: inputKeyboard, scan: scan, flags: flags}
	if flags&keyScancode != 0 {
		// Fill in the virtual-key code too. Windows itself goes by the scan
		// code, but some apps (and Wine) look at wVk, which is otherwise 0.
		sc := uintptr(scan)
		if flags&keyExtended != 0 {
			sc |= 0xE000
		}
		vk, _, _ := pMapVirtualKey.Call(sc, 3) // MAPVK_VSC_TO_VK_EX
		in.vk = uint16(vk)
	}
	if r, _, err := pSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in)); r == 0 {
		return fmt.Errorf("SendInput: %v", err)
	}
	return nil
}

func (s *gdiScreen) Input(ev desktopInput) error {
	if err := s.syncDesktop(); err != nil {
		return err
	}
	abs := func() (int32, int32) {
		// Normalised 0..65535 across the virtual desktop.
		return int32(clampF(ev.X) * 65535), int32(clampF(ev.Y) * 65535)
	}
	move := uint32(mouseMove | mouseAbsolute | mouseVirtualDesk)
	switch ev.T {
	case "mm":
		x, y := abs()
		return sendMouse(x, y, 0, move)
	case "md", "mu":
		down := ev.T == "md"
		flag := map[int][2]uint32{0: {mouseLeftDown, mouseLeftUp}, 1: {mouseMiddleDown, mouseMiddleUp}, 2: {mouseRightDown, mouseRightUp}}[ev.B]
		f := flag[1]
		if down {
			f = flag[0]
		}
		x, y := abs()
		return sendMouse(x, y, 0, move|f)
	case "wh":
		if ev.DY != 0 {
			return sendMouse(0, 0, uint32(int32(-ev.DY*120)), mouseWheel)
		}
		if ev.DX != 0 {
			return sendMouse(0, 0, uint32(int32(ev.DX*120)), mouseHWheel)
		}
	case "kd", "ku":
		k, ok := keyMap[ev.Code]
		if !ok {
			return nil
		}
		f := uint32(keyScancode)
		if k.ext {
			f |= keyExtended
		}
		if ev.T == "ku" {
			f |= keyUp
		}
		return sendKey(k.scan, f)
	case "text":
		for _, u := range windows.StringToUTF16(ev.S) {
			if u == 0 {
				break
			}
			if u == '\n' {
				sendKey(0x1C, keyScancode)
				sendKey(0x1C, keyScancode|keyUp)
				continue
			}
			if u == '\r' {
				continue
			}
			sendKey(u, keyUnicode)
			sendKey(u, keyUnicode|keyUp)
		}
	case "cad":
		// Only a service may generate the Secure Attention Sequence; ask the
		// parent agent service to do it via our stdout.
		fmt.Fprintln(os.Stdout, "@@SAS")
	}
	return nil
}

func (s *gdiScreen) Close() {
	s.freeBitmap()
	if s.desk != 0 {
		pCloseDesktop.Call(s.desk)
	}
}

func clampF(f float64) float64 { return max(0, min(1, f)) }

// ---------- service side: launch the helper in the interactive session ----------

// activeSession prefers the physical console; if nobody is on it but an RDP
// session is active, use that instead.
func activeSession() (uint32, error) {
	console := windows.WTSGetActiveConsoleSessionId()
	var infos *windows.WTS_SESSION_INFO
	var n uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &infos, &n); err == nil {
		defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(infos)))
		list := unsafe.Slice(infos, n)
		consoleActive := false
		var rdp uint32 = 0xFFFFFFFF
		for _, si := range list {
			if si.State != windows.WTSActive {
				continue
			}
			if si.SessionID == console {
				consoleActive = true
			} else if si.SessionID != 0 && rdp == 0xFFFFFFFF {
				rdp = si.SessionID
			}
		}
		if !consoleActive && rdp != 0xFFFFFFFF {
			return rdp, nil
		}
	}
	if console == 0xFFFFFFFF {
		return 0, errors.New("no interactive session (is a monitor/console attached?)")
	}
	return console, nil
}

func launchDesktopHelper(ctx context.Context, sid string, dial func() (*websocket.Conn, error), onSAS func()) error {
	session, err := activeSession()
	if err != nil {
		return err
	}
	// Our own SYSTEM token, re-targeted at the user's session. Running as
	// SYSTEM (not as the user) lets the helper see the login screen and UAC
	// prompts on the secure desktop.
	var self windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ALL_ACCESS, &self); err != nil {
		return fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer self.Close()
	var tok windows.Token
	if err := windows.DuplicateTokenEx(self, windows.MAXIMUM_ALLOWED, nil, windows.SecurityImpersonation, windows.TokenPrimary, &tok); err != nil {
		return fmt.Errorf("DuplicateTokenEx: %w", err)
	}
	defer tok.Close()
	if err := windows.SetTokenInformation(tok, windows.TokenSessionId, (*byte)(unsafe.Pointer(&session)), 4); err != nil {
		return fmt.Errorf("SetTokenInformation(session %d): %w", session, err)
	}

	// Pipe for the helper's stdout/stderr (logs and @@SAS requests).
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), InheritHandle: 1}
	var rd, wr windows.Handle
	if err := windows.CreatePipe(&rd, &wr, &sa, 0); err != nil {
		return err
	}
	windows.SetHandleInformation(rd, windows.HANDLE_FLAG_INHERIT, 0)

	exe, _ := os.Executable()
	cmdline, _ := windows.UTF16PtrFromString(fmt.Sprintf(`"%s" desktop-helper %s`, exe, sid))
	desktop, _ := windows.UTF16PtrFromString(`winsta0\default`)
	si := windows.StartupInfo{
		Cb:         uint32(unsafe.Sizeof(windows.StartupInfo{})),
		Desktop:    desktop,
		Flags:      windows.STARTF_USESTDHANDLES | windows.STARTF_USESHOWWINDOW,
		ShowWindow: windows.SW_HIDE,
		StdOutput:  wr,
		StdErr:     wr,
	}
	var pi windows.ProcessInformation
	err = windows.CreateProcessAsUser(tok, nil, cmdline, nil, nil, true, windows.CREATE_NO_WINDOW|windows.CREATE_UNICODE_ENVIRONMENT, nil, nil, &si, &pi)
	windows.CloseHandle(wr)
	if err != nil {
		windows.CloseHandle(rd)
		return fmt.Errorf("CreateProcessAsUser: %w", err)
	}
	windows.CloseHandle(pi.Thread)
	defer windows.CloseHandle(pi.Process)
	log.Printf("desktop %s: helper pid %d in session %d", sid, pi.ProcessId, session)

	go func() {
		f := os.NewFile(uintptr(rd), "desktop-helper")
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "@@SAS" {
				if onSAS != nil {
					onSAS()
				}
				continue
			}
			log.Printf("desktop-helper: %s", line)
		}
	}()

	done := make(chan struct{})
	go func() {
		windows.WaitForSingleObject(pi.Process, windows.INFINITE)
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		windows.TerminateProcess(pi.Process, 1)
		<-done
	}
	var code uint32
	windows.GetExitCodeProcess(pi.Process, &code)
	if code != 0 {
		return fmt.Errorf("helper exited with code %d", code)
	}
	return nil
}

// sendSAS simulates Ctrl+Alt+Del. Windows only allows this from a service and
// only when the SoftwareSASGeneration policy permits it, so we enable it for
// services (value 1) if it isn't already set.
func sendSAS() {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err == nil {
		if v, _, err := k.GetIntegerValue("SoftwareSASGeneration"); err != nil || v == 0 {
			k.SetDWordValue("SoftwareSASGeneration", 1)
		}
		k.Close()
	}
	if err := pSendSAS.Find(); err != nil {
		log.Printf("SendSAS unavailable: %v", err)
		return
	}
	pSendSAS.Call(0) // AsUser = FALSE (we are the service)
}
