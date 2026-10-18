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

/*
 * Link-preview assets.
 *
 * og.png and favicon.ico are written into web/dist at boot rather than committed
 * as binaries. Both are derived from logo.png, which is already the source of
 * truth for the mark — a committed copy would be a second thing to keep in step,
 * and this way a new logo shows up in link previews with no second edit.
 *
 * Standard library only. The obvious alternative is golang.org/x/image to scale
 * and to draw a font onto the card; x/image is not a dependency of this project
 * and pulling it in for two images would make every build of a self-hosted
 * server download it. Nearest-neighbour scaling is enough here: the mark is flat
 * colour and geometric, so there is nothing in it for a better filter to fix.
 *
 * The card carries no text. og:title carries the file's name and every chat app
 * renders it large, so the image only has to be the brand. Drawing the name into
 * the pixels as well would mean a font, and would then be a second copy of a
 * filename to keep correct.
 */

// Preview card dimensions. 1200x630 is the size Facebook, WhatsApp and Slack
// all ask for; anything else gets letterboxed or cropped by at least one of them.
const (
	previewWidth  = 1200
	previewHeight = 630
	markSize      = 220
)

// ensurePreviewAssets writes og.png and favicon.ico next to index.html.
func ensurePreviewAssets(resolve func() (string, error)) error {
	distDir, err := resolve()
	if err != nil {
		return err
	}

	logo, err := loadLogo(filepath.Join(distDir, "logo.png"))
	if err != nil {
		return fmt.Errorf("load logo: %w", err)
	}

	if err := os.WriteFile(filepath.Join(distDir, "og.png"), buildPreviewCard(logo), 0o644); err != nil {
		return fmt.Errorf("write og.png: %w", err)
	}
	if err := os.WriteFile(filepath.Join(distDir, "favicon.ico"), buildFavicon(logo), 0o644); err != nil {
		return fmt.Errorf("write favicon.ico: %w", err)
	}
	return nil
}

func loadLogo(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return png.Decode(f)
}

/*
 * buildPreviewCard draws the card: a pale blue field, a white panel with a
 * rounded corner and a hairline edge, and the mark centred on it.
 *
 * The panel is what stops the card reading as an empty blue rectangle on a
 * phone. Chat apps crop the image to whatever aspect their bubble happens to be,
 * and a mark floating on flat colour loses its edges in that crop; a card
 * inside the card keeps a defined shape at any crop.
 */
func buildPreviewCard(logo image.Image) []byte {
	img := image.NewRGBA(image.Rect(0, 0, previewWidth, previewHeight))

	// Field.
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.RGBA{0xf0, 0xf9, 0xff, 0xff}}, image.Point{}, draw.Src)

	// Panel, inset far enough that a horizontal crop still shows all four
	// corners of it.
	const inset = 48
	panel := image.Rect(inset, inset, previewWidth-inset, previewHeight-inset)
	fillRounded(img, panel, 36, color.RGBA{0xff, 0xff, 0xff, 0xff})

	// Hairline, drawn just inside the panel so it follows the same curve.
	strokeRounded(img, panel, 36, color.RGBA{0xde, 0xe2, 0xe6, 0xff})

	mark := scale(logo, markSize, markSize)
	x := previewWidth/2 - markSize/2
	y := previewHeight/2 - markSize/2
	draw.Draw(img, image.Rect(x, y, x+markSize, y+markSize), mark, image.Point{}, draw.Over)

	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

/*
 * fillRounded fills a rectangle with rounded corners.
 *
 * Kept in the integer grid and tested per pixel rather than done with a path and
 * a rasteriser, because the whole of this file is one shape and a radius: there
 * is nothing here that a general path walk would make shorter.
 */
func fillRounded(dst *image.RGBA, r image.Rectangle, radius int, c color.RGBA) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if !insideRounded(x, y, r, radius) {
				continue
			}
			dst.SetRGBA(x, y, c)
		}
	}
}

func strokeRounded(dst *image.RGBA, r image.Rectangle, radius int, c color.RGBA) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if !insideRounded(x, y, r, radius) {
				continue
			}
			// Only the pixels that touch the outside are the edge.
			for _, o := range []image.Point{{x - 1, y}, {x + 1, y}, {x, y - 1}, {x, y + 1}} {
				if !o.In(r) || !insideRounded(o.X, o.Y, r, radius) {
					dst.SetRGBA(x, y, c)
					break
				}
			}
		}
	}
}

// insideRounded reports whether a pixel falls inside a rounded rectangle.
func insideRounded(px, py int, r image.Rectangle, radius int) bool {
	if !image.Pt(px, py).In(r) {
		return false
	}
	cx := func(x int) int {
		switch {
		case x < r.Min.X+radius:
			return r.Min.X + radius
		case x > r.Max.X-1-radius:
			return r.Max.X - 1 - radius
		default:
			return x
		}
	}
	cy := func(y int) int {
		switch {
		case y < r.Min.Y+radius:
			return r.Min.Y + radius
		case y > r.Max.Y-1-radius:
			return r.Max.Y - 1 - radius
		default:
			return y
		}
	}
	dx, dy := float64(px-cx(px)), float64(py-cy(py))
	return dx*dx+dy*dy <= float64(radius*radius)
}

/*
 * scale resamples with a box filter, which is the right cheap choice here: the
 * mark is being reduced slightly more often than enlarged, and a box filter
 * avoids the aliasing that nearest-neighbour shows on the diagonal edges and the
 * inner cut-out, which are the parts the eye checks first.
 */
func scale(src image.Image, w, h int) image.Image {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		y0 := b.Min.Y + y*b.Dy()/h
		y1 := b.Min.Y + (y+1)*b.Dy()/h
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < w; x++ {
			x0 := b.Min.X + x*b.Dx()/w
			x1 := b.Min.X + (x+1)*b.Dx()/w
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var rs, gs, bs, as, n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					r, g, bb, a := src.At(sx, sy).RGBA()
					rs += uint64(r)
					gs += uint64(g)
					bs += uint64(bb)
					as += uint64(a)
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(rs / n >> 8), G: uint8(gs / n >> 8),
				B: uint8(bs / n >> 8), A: uint8(as / n >> 8),
			})
		}
	}
	return dst
}

/*
 * buildFavicon writes an ICO holding 16, 32 and 48 pixel PNG images.
 *
 * A PNG payload inside an ICO is what every current browser reads, and it is
 * what lets one file carry three sizes — the alternative is BMP-encoded frames,
 * which is more format for no visible gain here.
 *
 * favicon.ico exists at all because several crawlers do not read the HTML for an
 * icon and simply request /favicon.ico by convention. Without the file they get
 * a 404 and fall back to nothing, and a preview card with no icon beside it is
 * easy to mistake for a link to nowhere.
 */
func buildFavicon(logo image.Image) []byte {
	sizes := []int{16, 32, 48}
	images := make([][]byte, len(sizes))
	for i, s := range sizes {
		var buf bytes.Buffer
		_ = png.Encode(&buf, scale(logo, s, s))
		images[i] = buf.Bytes()
	}

	var out bytes.Buffer
	// ICONDIR: reserved, type (1 = icon), image count.
	_ = binary.Write(&out, binary.LittleEndian, uint16(0))
	_ = binary.Write(&out, binary.LittleEndian, uint16(1))
	_ = binary.Write(&out, binary.LittleEndian, uint16(len(sizes)))

	// ICONDIRENTRY per image. Width and height are single bytes, and 256 is
	// written as 0; the sizes here stop short of that anyway.
	offset := 6 + 16*len(sizes)
	for i, s := range sizes {
		d := s
		if d > 255 {
			d = 256
		}
		_ = binary.Write(&out, binary.LittleEndian, uint8(d))
		_ = binary.Write(&out, binary.LittleEndian, uint8(d))
		_ = binary.Write(&out, binary.LittleEndian, uint8(0))  // palette size
		_ = binary.Write(&out, binary.LittleEndian, uint8(0))  // reserved
		_ = binary.Write(&out, binary.LittleEndian, uint16(1)) // colour planes
		_ = binary.Write(&out, binary.LittleEndian, uint16(32))
		_ = binary.Write(&out, binary.LittleEndian, uint32(len(images[i])))
		_ = binary.Write(&out, binary.LittleEndian, uint32(offset))
		offset += len(images[i])
	}

	for _, img := range images {
		out.Write(img)
	}
	return out.Bytes()
}
