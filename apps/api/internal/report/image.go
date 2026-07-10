package report

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/jpeg"
	"math"

	"golang.org/x/image/draw"
)

// maxLongEdgePx is the longest-edge cap for photos embedded in a report PDF
// (docs/09: "target ~1200px long edge, ~80 quality" — a month of full-size
// photos would produce a 500 MB PDF).
const maxLongEdgePx = 1200

// jpegQuality is the re-encode quality for downscaled photos (docs/09).
const jpegQuality = 80

// DownscaleJPEG decodes raw photo bytes (a plain JPEG first; falling back to
// the standard library's format-sniffing image.Decode for anything
// image/jpeg's decoder rejects on its own), scales the longest edge down to
// at most maxLongEdgePx using a high-quality Catmull-Rom filter — never
// upscaling a photo already under the cap — and re-encodes as JPEG at
// jpegQuality. The result is returned as a data URI ready for an <img src>
// in the report template (RenderHTML's safeURL trusts exactly this shape).
func DownscaleJPEG(raw []byte) (string, error) {
	src, jpegErr := jpeg.Decode(bytes.NewReader(raw))
	if jpegErr != nil {
		var decodeErr error
		src, _, decodeErr = image.Decode(bytes.NewReader(raw))
		if decodeErr != nil {
			return "", fmt.Errorf("decoding photo: %w", jpegErr)
		}
	}

	scaled := scaleToLongEdge(src, maxLongEdgePx)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return "", fmt.Errorf("encoding downscaled photo: %w", err)
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// scaleToLongEdge returns src unchanged if its longest edge is already
// ≤maxEdge (never upscale — a small evidence photo stays exactly as
// captured). Otherwise it returns a copy scaled so the longest edge equals
// maxEdge, aspect ratio preserved, using draw.CatmullRom (docs/09 photo
// treatment: "no stretching").
func scaleToLongEdge(src image.Image, maxEdge int) image.Image {
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	longEdge := w
	if h > longEdge {
		longEdge = h
	}
	if longEdge <= maxEdge {
		return src
	}

	scale := float64(maxEdge) / float64(longEdge)
	newW := int(math.Round(float64(w) * scale))
	newH := int(math.Round(float64(h) * scale))
	if newW < 1 {
		newW = 1
	}
	if newH < 1 {
		newH = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, bounds, draw.Src, nil)
	return dst
}
