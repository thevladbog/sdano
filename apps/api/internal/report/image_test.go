package report

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

// buildTestJPEG synthesizes a w×h JPEG in-code (no fixture files) so the
// test is self-contained and deterministic.
func buildTestJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encoding test jpeg: %v", err)
	}
	return buf.Bytes()
}

// decodeDataURI strips the "data:image/jpeg;base64," prefix, decodes the
// base64 payload, and decodes the resulting JPEG bytes.
func decodeDataURI(t *testing.T, dataURI string) image.Image {
	t.Helper()
	const prefix = "data:image/jpeg;base64,"
	if !strings.HasPrefix(dataURI, prefix) {
		end := min(len(dataURI), 40)
		t.Fatalf("data URI missing prefix %q; got %q...", prefix, dataURI[:end])
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(dataURI, prefix))
	if err != nil {
		t.Fatalf("decoding base64 payload: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decoding downscaled jpeg: %v", err)
	}
	return img
}

// TestDownscaleJPEGScalesLongEdgeDownPreservingAspect proves a large photo
// (2400×1200) is scaled so its long edge is ≤1200px, with the short edge
// shrunk proportionally (600) rather than distorted.
func TestDownscaleJPEGScalesLongEdgeDownPreservingAspect(t *testing.T) {
	raw := buildTestJPEG(t, 2400, 1200)
	dataURI, err := DownscaleJPEG(raw)
	if err != nil {
		t.Fatalf("DownscaleJPEG: %v", err)
	}
	if !strings.HasPrefix(dataURI, "data:image/jpeg;base64,") {
		t.Fatalf("data URI missing expected prefix: %q...", dataURI[:min(len(dataURI), 40)])
	}
	img := decodeDataURI(t, dataURI)
	b := img.Bounds()
	if b.Dx() > 1200 || b.Dy() > 1200 {
		t.Fatalf("bounds %dx%d exceed 1200 on the long edge", b.Dx(), b.Dy())
	}
	if b.Dx() != 1200 || b.Dy() != 600 {
		t.Fatalf("bounds = %dx%d; want 1200x600 (aspect preserved)", b.Dx(), b.Dy())
	}
}

// TestDownscaleJPEGPassesThroughSmallImageUnupscaled proves a photo already
// under the 1200px cap is never upscaled: bounds stay exactly as-is.
func TestDownscaleJPEGPassesThroughSmallImageUnupscaled(t *testing.T) {
	raw := buildTestJPEG(t, 100, 80)
	dataURI, err := DownscaleJPEG(raw)
	if err != nil {
		t.Fatalf("DownscaleJPEG: %v", err)
	}
	img := decodeDataURI(t, dataURI)
	b := img.Bounds()
	if b.Dx() != 100 || b.Dy() != 80 {
		t.Fatalf("bounds = %dx%d; want unchanged 100x80 (never upscale)", b.Dx(), b.Dy())
	}
}

// TestDownscaleJPEGAcceptsPNG proves the image.Decode fallback is live: a
// PNG that slipped past the jpeg-only presign contract (S3 never validates
// the uploaded bytes) still renders into the report as a JPEG data URI
// instead of failing the whole report — evidence is sacred, and a decodable
// photo must never be the reason a tenant's report dies after 3 attempts.
func TestDownscaleJPEGAcceptsPNG(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 60, 40))
	for y := 0; y < 40; y++ {
		for x := 0; x < 60; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding test png: %v", err)
	}

	dataURI, err := DownscaleJPEG(buf.Bytes())
	if err != nil {
		t.Fatalf("DownscaleJPEG(png): %v", err)
	}
	out := decodeDataURI(t, dataURI)
	b := out.Bounds()
	if b.Dx() != 60 || b.Dy() != 40 {
		t.Fatalf("bounds = %dx%d; want unchanged 60x40", b.Dx(), b.Dy())
	}
}

// TestDownscaleJPEGRejectsGarbage proves garbage bytes fail loudly rather
// than silently producing a blank or corrupt image.
func TestDownscaleJPEGRejectsGarbage(t *testing.T) {
	if _, err := DownscaleJPEG([]byte("not an image, just garbage bytes well past any header")); err == nil {
		t.Fatal("DownscaleJPEG(garbage): want error, got nil")
	}
}
