// Package imageproc renders a small, upright copy of an uploaded image.
//
// It exists because serving the original breaks down on a slow link. A phone
// photo is several megabytes and often 4000-8000 px on a side; sending all of
// that so the browser can draw it 800 px wide spends bandwidth and decoder time
// on detail no screen can show. Every self-hosted front end that feels fast
// does the same thing about it: generate a smaller copy once, keep it, and hand
// the browser that instead. FileBrowser serves a 256 px thumbnail or a 1080 px
// "big"; Nextcloud keeps a max preview and derives every smaller size from it;
// oCIS keeps a resolution ladder. This package is that idea, reduced to the one
// step they all share — decode, orient, downscale, re-encode.
//
// Orientation is not cosmetic. A JPEG straight from a phone stores its rotation
// in an EXIF tag while its pixels lie the other way. Browsers honour that tag
// when they display the original, so a re-encode that dropped the tag without
// baking the rotation into the pixels would turn every portrait photo on its
// side. AutoOrientation reads the tag and rotates the pixels, so what this
// package writes is upright with no metadata needed — which also means the
// preview carries no GPS or camera EXIF.
package imageproc

import (
	"bytes"
	"errors"
	"image"
	"io"
	"strings"

	"github.com/disintegration/imaging"

	// Decoders beyond the standard library's JPEG/PNG/GIF set. Importing them
	// for their side effect registers them with image.Decode, which is what
	// both DecodeConfig and imaging.Decode consult.
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

const (
	// MaxSourceDimension caps the width or height this package will decode. A
	// 12000×12000 bitmap is over half a gigabyte in memory, and a phone upload
	// should not be able to take the server down just by being large. The
	// storage layer still serves such an original; it is only the generated
	// preview that gives up.
	MaxSourceDimension = 12000

	// MinWidth and MaxWidth bound a requested preview width. Below the floor a
	// downscale is not worth the round trip; above the ceiling there are more
	// pixels than any screen shows.
	MinWidth = 64
	MaxWidth = 4096

	// jpegQuality trades a little fidelity for a much smaller file, which is
	// the entire point of a preview. 82 is the range photo tools use by
	// default and is visually indistinguishable at preview scale.
	jpegQuality = 82
)

// ErrUnsupported is returned for a source type this package will not render.
var ErrUnsupported = errors.New("imageproc: unsupported image type")

// ErrTooLarge is returned when the source's dimensions exceed
// MaxSourceDimension.
var ErrTooLarge = errors.New("imageproc: image is too large to preview")

// Renderer renders previews, at most a fixed number at a time.
//
// Decoding a 12-megapixel photo is CPU- and memory-hungry, so a folder of
// images opened at once would otherwise start one decode per request and put
// every core (and a multiple of the host's RAM in decoded bitmaps) to work at
// the same instant. The limit is the same idea as Nextcloud's preview
// concurrency and oCIS's THUMBNAILS_MAX_CONCURRENT_REQUESTS: past it, callers
// wait rather than pile on.
type Renderer struct {
	slots chan struct{}
}

// NewRenderer builds a Renderer that renders at most concurrency images at
// once. A value below one is treated as one.
func NewRenderer(concurrency int) *Renderer {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Renderer{slots: make(chan struct{}, concurrency)}
}

func (r *Renderer) acquire() { r.slots <- struct{}{} }
func (r *Renderer) release() { <-r.slots }

// Supported reports whether Render can handle a source MIME type.
func Supported(mime string) bool {
	switch mimeBase(mime) {
	case "image/jpeg", "image/png", "image/webp", "image/bmp", "image/tiff":
		return true
	}
	// GIF is deliberately absent: it is usually animated, imaging decodes only
	// its first frame, and re-encoding would silently turn a moving picture
	// into a still. SVG is absent for the opposite reason — it is a document
	// that the browser already renders, not a raster bitmap to resize.
	return false
}

// OutputMime is the type Render produces for a given source type. PNG keeps its
// alpha channel; everything else becomes JPEG, which is smaller for the
// photographic content that makes up most of a media library.
func OutputMime(srcMime string) string {
	if mimeBase(srcMime) == "image/png" {
		return "image/png"
	}
	return "image/jpeg"
}

// NormalizeWidth validates a requested preview width, returning 0 for anything
// outside the supported range. A 0 tells the caller to serve the original.
func NormalizeWidth(width int) int {
	if width < MinWidth || width > MaxWidth {
		return 0
	}
	return width
}

func mimeBase(mime string) string {
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	return strings.ToLower(strings.TrimSpace(mime))
}

// Render decodes src, applies its EXIF orientation, scales it to fit within
// maxDim×maxDim (never enlarging a smaller image), and re-encodes it. It
// returns the encoded bytes and their MIME type.
func (r *Renderer) Render(src io.Reader, mime string, maxDim int) ([]byte, string, error) {
	if !Supported(mime) {
		return nil, "", ErrUnsupported
	}
	maxDim = NormalizeWidth(maxDim)
	if maxDim == 0 {
		return nil, "", ErrUnsupported
	}

	// The source is buffered because it is read twice — once to read the
	// dimensions before committing memory to a decode, once to decode. The
	// caller caps how big it lets this get (see maxThumbSourceBytes).
	raw, err := io.ReadAll(src)
	if err != nil {
		return nil, "", err
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, "", err
	}
	if cfg.Width > MaxSourceDimension || cfg.Height > MaxSourceDimension {
		return nil, "", ErrTooLarge
	}

	r.acquire()
	defer r.release()

	img, err := imaging.Decode(bytes.NewReader(raw), imaging.AutoOrientation(true))
	if err != nil {
		return nil, "", err
	}

	if b := img.Bounds(); b.Dx() > maxDim || b.Dy() > maxDim {
		// CatmullRom is a good middle ground for a large downscale: sharper
		// than a box average, cheaper than Lanczos, and free of the ring it
		// would put around edges.
		img = imaging.Fit(img, maxDim, maxDim, imaging.CatmullRom)
	}

	outMime := OutputMime(mime)
	var buf bytes.Buffer
	if outMime == "image/png" {
		err = imaging.Encode(&buf, img, imaging.PNG)
	} else {
		err = imaging.Encode(&buf, img, imaging.JPEG, imaging.JPEGQuality(jpegQuality))
	}
	if err != nil {
		return nil, "", err
	}
	return buf.Bytes(), outMime, nil
}
