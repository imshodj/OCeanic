package agent

import (
	"image"
	"testing"
)

func TestChangedStripsOddSizes(t *testing.T) {
	for _, w := range []int{800, 1366, 1440, 1600, 1680, 1920, 37} {
		h := 77
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		e := &frameEncoder{quality: 50}
		// Full frame must cover every pixel exactly once.
		area := 0
		for _, r := range e.changedStrips(img) {
			area += r.Dx() * r.Dy()
		}
		if area != w*h {
			t.Fatalf("w=%d: full frame covers %d px, want %d", w, area, w*h)
		}
		e.encode(img)
		// Change one pixel in the last (partial) column: exactly one strip.
		img.Pix[(h-1)*img.Stride+(w-1)*4] = 255
		rs := e.changedStrips(img)
		if len(rs) != 1 || !image.Pt(w-1, h-1).In(rs[0]) {
			t.Fatalf("w=%d: change at right edge gave %v", w, rs)
		}
		if e.encode(img) == nil {
			t.Fatalf("w=%d: encode returned nothing", w)
		}
		if e.encode(img) != nil {
			t.Fatalf("w=%d: unchanged frame should encode to nil", w)
		}
	}
}
