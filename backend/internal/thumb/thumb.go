// Package thumb generates image previews.
//
// It exists as its own package because the decode step is the one place in the
// product that runs an attacker-supplied parser over attacker-supplied bytes,
// and the guards that make that safe belong next to it rather than scattered
// through a worker.
//
// THE OUTPUT IS image/jpeg, NOT image/webp.
//
// Plan 02 §9, registry.Preview, storage.PresignOptions and drive's presignThumb
// all committed to WebP on the strength of one new dependency. That dependency
// cannot produce it: golang.org/x/image/webp exposes Decode and DecodeConfig and
// no encoder at all. The alternatives were a second, young, pure-Go VP8L encoder
// running inside a worker that decodes hostile input, or writing one.
//
// The security property was never "WebP". It is that the thumbnail — the one
// object Drive serves INLINE from a presigned URL, where no CSP header can
// travel — has a SINGLE, KNOWN-SAFE type that the server produced itself.
// image/jpeg satisfies that exactly as well, it is in the inline allowlist
// already, and it is the right format for the photographic content that makes
// up almost every thumbnail.
package thumb

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"

	"golang.org/x/image/draw"
	// Registers the WebP decoder. Decode-only, which is the whole reason the
	// output below is JPEG.
	_ "golang.org/x/image/webp"
)

// MediaType is what every thumbnail this package produces is. One value, so a
// caller never has to ask.
const MediaType = "image/jpeg"

// Extension is the suffix a thumbnail key carries. drive's presignThumb derives
// the served content type from it against a closed map, so a key written with a
// different extension serves nothing rather than serving the wrong type.
const Extension = ".jpg"

// MaxPixels bounds what will be DECODED, and it is the guard that matters.
//
// A decompression bomb is a small file that declares an enormous canvas: a 20KB
// PNG can declare 40000x40000, and image.Decode will try to allocate
// 40000*40000*4 bytes — 6.4GB — before anything gets a chance to notice. The
// worker is a shared process, so that is not one failed thumbnail, it is every
// consumer in it.
//
// 40 megapixels is roughly a 50-megapixel camera's raw output and far past
// anything a preview needs.
const MaxPixels = 40 << 20

// MaxSourceBytes bounds what will be READ from storage. DecodeConfig only needs
// a header, but Decode needs the file; this stops a 2GB "image" from being
// pulled into the worker's heap at all.
const MaxSourceBytes = 64 << 20

// Size is the longest edge of the generated preview. Thumbnails are rendered in
// a grid; anything larger is bytes nobody looks at.
const Size = 512

// Quality is the JPEG quality. 82 is the usual knee: visually indistinguishable
// from 95 at thumbnail scale and roughly half the bytes.
const Quality = 82

// ErrNoPreview means this object has no preview and never will — a type nothing
// can decode, or bytes that are not the image they claim to be. It is not a
// failure: most files have no thumbnail, and the caller records "none" rather
// than retrying forever.
var ErrNoPreview = errors.New("thumb: no preview available for these bytes")

// ErrTooLarge means the declared canvas is past MaxPixels. Also permanent: the
// same bytes will declare the same canvas next time.
var ErrTooLarge = errors.New("thumb: image dimensions exceed the decode budget")

// Preview is a generated thumbnail.
type Preview struct {
	Bytes  []byte
	Width  int
	Height int
}

// Generate reads an image and returns a downscaled JPEG.
//
// The order of operations is the security control and it is not rearrangeable:
//
//  1. read at most MaxSourceBytes, so a huge object cannot be pulled into RAM;
//  2. DecodeConfig, which reads only a header and allocates nothing;
//  3. CHECK THE DIMENSIONS;
//  4. only then Decode, which allocates the pixels.
//
// Doing 4 before 3 is the decompression bomb. It is a one-line reordering and it
// takes the whole worker down.
func Generate(src io.Reader) (Preview, error) {
	raw, err := io.ReadAll(io.LimitReader(src, MaxSourceBytes+1))
	if err != nil {
		return Preview{}, fmt.Errorf("read image: %w", err)
	}
	if len(raw) > MaxSourceBytes {
		return Preview{}, fmt.Errorf("%w: source is larger than %d bytes", ErrTooLarge, MaxSourceBytes)
	}

	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		// Not decodable as any registered format. Permanent.
		return Preview{}, fmt.Errorf("%w: %v", ErrNoPreview, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return Preview{}, fmt.Errorf("%w: %s declares a %dx%d canvas", ErrNoPreview, format, cfg.Width, cfg.Height)
	}
	// int64 on purpose: two int dimensions can overflow int on a 32-bit build,
	// and an overflowed product compares as small.
	if int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return Preview{}, fmt.Errorf("%w: %s declares %dx%d (%d pixels), the budget is %d",
			ErrTooLarge, format, cfg.Width, cfg.Height, int64(cfg.Width)*int64(cfg.Height), MaxPixels)
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		// The header parsed and the pixels did not. Still permanent: retrying
		// re-reads the same truncated or corrupt bytes.
		return Preview{}, fmt.Errorf("%w: %v", ErrNoPreview, err)
	}

	dst := fit(img.Bounds().Dx(), img.Bounds().Dy())
	out := image.NewRGBA(image.Rect(0, 0, dst.X, dst.Y))
	// CatmullRom rather than NearestNeighbor: a thumbnail is looked at, and the
	// stdlib's image/draw has no resampler at all — which is the second reason
	// x/image is a dependency.
	draw.CatmullRom.Scale(out, out.Bounds(), img, img.Bounds(), draw.Over, nil)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: Quality}); err != nil {
		return Preview{}, fmt.Errorf("encode thumbnail: %w", err)
	}
	return Preview{Bytes: buf.Bytes(), Width: dst.X, Height: dst.Y}, nil
}

// fit scales (w, h) so the longest edge is Size, never enlarging: upscaling a
// 32x32 icon to 512 produces a blurry 40KB object in place of a crisp 2KB one.
func fit(w, h int) image.Point {
	if w <= Size && h <= Size {
		return image.Point{X: w, Y: h}
	}
	if w >= h {
		return image.Point{X: Size, Y: max(1, h*Size/w)}
	}
	return image.Point{X: max(1, w*Size/h), Y: Size}
}

// Decodable reports whether a content type is one this package can read.
//
// A closed set, checked before anything is fetched from storage: it is what
// stops the worker downloading a 60MB video to discover it cannot decode it.
// SVG is deliberately absent — it is an XML document that can carry script, and
// "it is an image" is exactly the reasoning that gets it into an allowlist.
func Decodable(contentType string) bool {
	switch contentType {
	case "image/jpeg", "image/png", "image/gif", "image/webp", "image/bmp":
		return true
	default:
		return false
	}
}

// Keep the stdlib decoders registered. image.Decode dispatches on the registry
// these init()s populate, and a missing blank import is a format that silently
// stops being decodable.
var (
	_ = png.Decode
	_ = gif.Decode
	_ = jpeg.Decode
)
