//go:build ignore

// genicon draws the Watchink app icon (an original mark: a play triangle
// leading a note of three lines on a rounded indigo tile) as a 1024x1024 PNG.
//
//	go run scripts/genicon.go build/appicon.png
//
// scripts/make-icns.sh turns the PNG into build/darwin/Watchink.icns.
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

const (
	size = 1024
	ss   = 4 // supersampling per axis, for anti-aliased edges
)

type rgb struct{ r, g, b float64 }

func mix(a, b rgb, t float64) rgb {
	return rgb{a.r + (b.r-a.r)*t, a.g + (b.g-a.g)*t, a.b + (b.b-a.b)*t}
}

// inRoundRect reports whether (x, y) is inside the rounded rectangle.
func inRoundRect(x, y, x0, y0, x1, y1, r float64) bool {
	if x < x0 || x > x1 || y < y0 || y > y1 {
		return false
	}
	cx := math.Max(x0+r, math.Min(x, x1-r))
	cy := math.Max(y0+r, math.Min(y, y1-r))
	return math.Hypot(x-cx, y-cy) <= r
}

// inTriangle reports whether p is inside triangle abc.
func inTriangle(px, py, ax, ay, bx, by, cx, cy float64) bool {
	d1 := (px-bx)*(ay-by) - (ax-bx)*(py-by)
	d2 := (px-cx)*(by-cy) - (bx-cx)*(py-cy)
	d3 := (px-ax)*(cy-ay) - (cx-ax)*(py-ay)
	neg := d1 < 0 || d2 < 0 || d3 < 0
	pos := d1 > 0 || d2 > 0 || d3 > 0
	return !(neg && pos)
}

// sample returns the colour and coverage of one point.
func sample(x, y float64) (rgb, float64) {
	// The tile follows the macOS icon grid: 824 px body with 100 px margins.
	if !inRoundRect(x, y, 100, 100, 924, 924, 185) {
		return rgb{}, 0
	}
	top, bottom := rgb{0.39, 0.40, 0.95}, rgb{0.20, 0.17, 0.62}
	c := mix(top, bottom, (y-100)/824)
	white := rgb{1, 1, 1}
	// Play triangle (the video).
	if inTriangle(x, y, 250, 290, 250, 530, 450, 410) {
		return white, 1
	}
	// Three note lines; the first sits beside the triangle (the trace).
	lines := [][4]float64{{510, 380, 780, 440}, {250, 600, 780, 660}, {250, 720, 620, 780}}
	for _, l := range lines {
		if inRoundRect(x, y, l[0], l[1], l[2], l[3], 30) {
			return white, 1
		}
	}
	// Timestamp dot ending the last line.
	if math.Hypot(x-730, y-750) <= 46 {
		return rgb{1.0, 0.80, 0.30}, 1
	}
	return c, 1
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run scripts/genicon.go <out.png>")
		os.Exit(2)
	}
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var acc rgb
			var cov float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					c, a := sample(float64(px)+(float64(sx)+0.5)/ss, float64(py)+(float64(sy)+0.5)/ss)
					acc.r += c.r * a
					acc.g += c.g * a
					acc.b += c.b * a
					cov += a
				}
			}
			if cov == 0 {
				continue
			}
			img.SetNRGBA(px, py, color.NRGBA{
				R: uint8(math.Round(acc.r / cov * 255)),
				G: uint8(math.Round(acc.g / cov * 255)),
				B: uint8(math.Round(acc.b / cov * 255)),
				A: uint8(math.Round(cov / (ss * ss) * 255)),
			})
		}
	}
	f, err := os.Create(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := png.Encode(f, img); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := f.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
