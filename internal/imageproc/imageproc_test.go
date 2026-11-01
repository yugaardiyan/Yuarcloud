package imageproc

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func gradient(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, 255})
		}
	}
	return img
}

func TestSupportedAndOutputMime(t *testing.T) {
	supported := []string{"image/jpeg", "image/png", "image/webp", "image/bmp", "image/tiff", "image/jpeg; charset=binary"}
	for _, m := range supported {
		if !Supported(m) {
			t.Errorf("Supported(%q) = false, want true", m)
		}
	}
	unsupported := []string{"image/gif", "image/svg+xml", "video/mp4", "application/pdf", ""}
	for _, m := range unsupported {
		if Supported(m) {
			t.Errorf("Supported(%q) = true, want false", m)
		}
	}
	if got := OutputMime("image/png"); got != "image/png" {
		t.Errorf("OutputMime(png) = %q, want image/png", got)
	}
	if got := OutputMime("image/jpeg"); got != "image/jpeg" {
		t.Errorf("OutputMime(jpeg) = %q, want image/jpeg", got)
	}
}

func TestNormalizeWidth(t *testing.T) {
	cases := map[int]int{
		0: 0, 10: 0, 63: 0, 64: 64, 2048: 2048, 4096: 4096, 4097: 0, 99999: 0,
	}
	for in, want := range cases {
		if got := NormalizeWidth(in); got != want {
			t.Errorf("NormalizeWidth(%d) = %d, want %d", in, got, want)
		}
	}
}

// A preview is smaller than its source and, for a JPEG source, is a JPEG.
func TestRenderDownscales(t *testing.T) {
	var src bytes.Buffer
	if err := jpeg.Encode(&src, gradient(800, 600), nil); err != nil {
		t.Fatalf("encode source: %v", err)
	}

	r := NewRenderer(2)
	out, mime, err := r.Render(bytes.NewReader(src.Bytes()), "image/jpeg", 64)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if mime != "image/jpeg" {
		t.Fatalf("mime = %q, want image/jpeg", mime)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if cfg.Width > 64 || cfg.Height > 64 {
		t.Fatalf("preview is %dx%d, want both <= 64", cfg.Width, cfg.Height)
	}
	if cfg.Width == 0 || cfg.Height == 0 {
		t.Fatalf("preview has zero dimension: %dx%d", cfg.Width, cfg.Height)
	}
	if len(out) >= src.Len() {
		t.Fatalf("preview (%d bytes) is not smaller than source (%d bytes)", len(out), src.Len())
	}
}

// A PNG source keeps its alpha channel and stays a PNG.
func TestRenderKeepsPNG(t *testing.T) {
	var src bytes.Buffer
	if err := png.Encode(&src, gradient(400, 400)); err != nil {
		t.Fatalf("encode source: %v", err)
	}
	out, mime, err := NewRenderer(1).Render(bytes.NewReader(src.Bytes()), "image/png", 128)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if mime != "image/png" {
		t.Fatalf("mime = %q, want image/png", mime)
	}
	if _, format, err := image.Decode(bytes.NewReader(out)); err != nil || format != "png" {
		t.Fatalf("decoded preview format = %q, err = %v", format, err)
	}
}

// An image already smaller than the target is not bloated up to it.
func TestRenderDoesNotEnlarge(t *testing.T) {
	var src bytes.Buffer
	if err := jpeg.Encode(&src, gradient(120, 90), nil); err != nil {
		t.Fatalf("encode source: %v", err)
	}
	out, _, err := NewRenderer(1).Render(bytes.NewReader(src.Bytes()), "image/jpeg", 2048)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if cfg.Width != 120 || cfg.Height != 90 {
		t.Fatalf("preview resized a small image to %dx%d", cfg.Width, cfg.Height)
	}
}

func TestRenderRejectsUnsupported(t *testing.T) {
	if _, _, err := NewRenderer(1).Render(bytes.NewReader(nil), "image/gif", 128); err == nil {
		t.Fatal("expected an error for image/gif")
	}
}
