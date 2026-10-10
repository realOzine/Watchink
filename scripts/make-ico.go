//go:build ignore

// make-ico turns build/appicon.png (1024x1024) into the multi-size Windows
// icon build/windows/Watchink.ico, using only the standard library so it runs
// on any development machine.
//
//	go run scripts/make-ico.go [build/appicon.png [build/windows/Watchink.ico]]
//
// Regenerate the PNG first with `node scripts/render-icon.mjs` (see `make icon`).
// The .ico is committed; scripts/build-windows.sh compiles it into the
// executables as icon resource 3, the id Wails loads the window icon from.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
)

// sizes are the icon sizes Explorer, the taskbar and the title bar pick from
// at 100 %–250 % scaling.
var sizes = []int{16, 20, 24, 32, 40, 48, 64, 128, 256}

func main() {
	src, dst := "build/appicon.png", "build/windows/Watchink.ico"
	if len(os.Args) > 1 {
		src = os.Args[1]
	}
	if len(os.Args) > 2 {
		dst = os.Args[2]
	}
	if err := run(src, dst); err != nil {
		fmt.Fprintln(os.Stderr, "make-ico:", err)
		os.Exit(1)
	}
	fmt.Println("wrote", dst)
}

func run(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	decoded, err := png.Decode(f)
	if err != nil {
		return fmt.Errorf("decode %s: %w", src, err)
	}
	b := decoded.Bounds()
	if b.Dx() != b.Dy() || b.Dx() < 256 {
		return fmt.Errorf("%s must be square and at least 256 px, got %dx%d", src, b.Dx(), b.Dy())
	}
	full := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(full, full.Bounds(), decoded, b.Min, draw.Src)

	images := make([][]byte, len(sizes))
	for i, size := range sizes {
		img := downscale(full, size)
		if size >= 256 {
			// Vista and later read PNG-compressed entries; 256 px must be one.
			var buf bytes.Buffer
			if err := png.Encode(&buf, img); err != nil {
				return err
			}
			images[i] = buf.Bytes()
		} else {
			images[i] = dib(img)
		}
	}

	// ICONDIR, one ICONDIRENTRY per image, then the image data.
	var out bytes.Buffer
	le := binary.LittleEndian
	binary.Write(&out, le, [3]uint16{0, 1, uint16(len(sizes))})
	offset := 6 + 16*len(sizes)
	for i, size := range sizes {
		dim := uint8(size) // 256 is stored as 0
		out.Write([]byte{dim, dim, 0, 0})
		binary.Write(&out, le, [2]uint16{1, 32}) // planes, bits per pixel
		binary.Write(&out, le, [2]uint32{uint32(len(images[i])), uint32(offset)})
		offset += len(images[i])
	}
	for _, data := range images {
		out.Write(data)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, out.Bytes(), 0o644)
}

// downscale resamples src to size x size by averaging the source pixels each
// destination pixel covers (area filter), weighting colour by alpha so the
// transparent corners do not darken the edge.
func downscale(src *image.NRGBA, size int) *image.NRGBA {
	n := src.Bounds().Dx()
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	scale := float64(n) / float64(size)
	for y := 0; y < size; y++ {
		y0, y1 := float64(y)*scale, float64(y+1)*scale
		for x := 0; x < size; x++ {
			x0, x1 := float64(x)*scale, float64(x+1)*scale
			var r, g, b, a, area float64
			for sy := int(y0); sy < n && float64(sy) < y1; sy++ {
				wy := overlap(float64(sy), y0, y1)
				for sx := int(x0); sx < n && float64(sx) < x1; sx++ {
					w := wy * overlap(float64(sx), x0, x1)
					c := src.NRGBAAt(sx, sy)
					alpha := float64(c.A) / 255
					r += float64(c.R) * alpha * w
					g += float64(c.G) * alpha * w
					b += float64(c.B) * alpha * w
					a += alpha * w
					area += w
				}
			}
			if a <= 0 || area <= 0 {
				continue
			}
			dst.SetNRGBA(x, y, color.NRGBA{
				R: clamp(r / a), G: clamp(g / a), B: clamp(b / a), A: clamp(255 * a / area),
			})
		}
	}
	return dst
}

// overlap is the length of [p, p+1) inside [lo, hi).
func overlap(p, lo, hi float64) float64 {
	a, b := p, p+1
	if a < lo {
		a = lo
	}
	if b > hi {
		b = hi
	}
	if b <= a {
		return 0
	}
	return b - a
}

func clamp(v float64) uint8 {
	switch {
	case v <= 0:
		return 0
	case v >= 255:
		return 255
	}
	return uint8(v + 0.5)
}

// dib encodes img as the device-independent bitmap an .ico entry holds: a
// BITMAPINFOHEADER whose height covers the colour bitmap plus the 1-bit AND
// mask, 32-bit BGRA rows bottom-up, then the mask (all zero: alpha decides).
func dib(img *image.NRGBA) []byte {
	size := img.Bounds().Dx()
	var out bytes.Buffer
	le := binary.LittleEndian
	binary.Write(&out, le, struct {
		Size          uint32
		Width, Height int32
		Planes, Bits  uint16
		Compression   uint32
		ImageSize     uint32
		XPPM, YPPM    int32
		Used, Needed  uint32
	}{Size: 40, Width: int32(size), Height: int32(2 * size), Planes: 1, Bits: 32, ImageSize: uint32(4 * size * size)})
	for y := size - 1; y >= 0; y-- {
		for x := 0; x < size; x++ {
			c := img.NRGBAAt(x, y)
			out.Write([]byte{c.B, c.G, c.R, c.A})
		}
	}
	maskRow := (size + 31) / 32 * 4 // rows are padded to 32 bits
	out.Write(make([]byte, maskRow*size))
	return out.Bytes()
}
