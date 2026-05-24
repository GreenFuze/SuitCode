//go:build systray

package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
)

// trayIcon returns a 32×32 green square PNG as bytes.
// This is a placeholder; replace with a branded asset when ready.
func trayIcon() []byte {
	const size = 32

	img := image.NewNRGBA(image.Rect(0, 0, size, size))

	// Tailwind green-500 fill with a slightly darker border.
	fill := color.NRGBA{R: 34, G: 197, B: 94, A: 255}
	border := color.NRGBA{R: 22, G: 163, B: 74, A: 255}

	for y := range size {
		for x := range size {
			if x == 0 || y == 0 || x == size-1 || y == size-1 {
				img.SetNRGBA(x, y, border)
			} else {
				img.SetNRGBA(x, y, fill)
			}
		}
	}

	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
