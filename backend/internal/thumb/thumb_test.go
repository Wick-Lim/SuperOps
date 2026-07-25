package thumb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestGenerateProducesJPEGAndDownscales(t *testing.T) {
	p, err := Generate(bytes.NewReader(pngOf(t, 1600, 800)))
	if err != nil {
		t.Fatal(err)
	}
	if p.Width != Size || p.Height != Size/2 {
		t.Errorf("thumbnail = %dx%d, want %dx%d (longest edge capped, aspect kept)",
			p.Width, p.Height, Size, Size/2)
	}

	// It really is a JPEG. The type is a security property: the thumbnail is the
	// one object Drive serves INLINE from a presigned URL, where no CSP header
	// can travel, so it has to be a single known-safe type the server produced.
	_, format, err := image.DecodeConfig(bytes.NewReader(p.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	if format != "jpeg" {
		t.Errorf("thumbnail format = %q, want jpeg", format)
	}
	if MediaType != "image/jpeg" {
		t.Errorf("MediaType = %q; it must match what Generate actually writes", MediaType)
	}
}

// Never enlarge: upscaling a 32x32 icon to 512 produces a blurry object several
// times the size of the original.
func TestGenerateNeverEnlarges(t *testing.T) {
	p, err := Generate(bytes.NewReader(pngOf(t, 32, 24)))
	if err != nil {
		t.Fatal(err)
	}
	if p.Width != 32 || p.Height != 24 {
		t.Errorf("small image scaled to %dx%d, want 32x24", p.Width, p.Height)
	}
}

// THE DECOMPRESSION BOMB, which is the reason this package exists as a package.
//
// A small PNG that declares an enormous canvas. image.Decode allocates
// width*height*4 bytes before anything can object — for 40000x40000 that is
// 6.4GB, in a worker shared with every other consumer. The guard has to run
// between DecodeConfig and Decode, and this test is what pins that ordering.
func TestGenerateRejectsADecompressionBombBeforeAllocating(t *testing.T) {
	// A VALID PNG header declaring 40000x40000, with no image data behind it.
	// Synthesised rather than encoded, because encoding one honestly would
	// allocate the 6.4GB this test exists to prevent — and the CRC is computed
	// rather than written down, so the file really does parse and the guard is
	// reached rather than the format check tripping first.
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})

	body := []byte{
		'I', 'H', 'D', 'R',
		0x00, 0x00, 0x9c, 0x40, // width  40000
		0x00, 0x00, 0x9c, 0x40, // height 40000
		8, 6, 0, 0, 0, // 8-bit RGBA: 40000*40000*4 = 6.4GB if decoded
	}
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(body)-4))
	buf.Write(body)
	_ = binary.Write(&buf, binary.BigEndian, crc32.ChecksumIEEE(body))

	_, err := Generate(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Fatal("a 40000x40000 declaration was accepted; Decode would allocate 6.4GB in a " +
			"process shared with every other consumer")
	}
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("rejected with %v, want ErrTooLarge — the dimension check must run between "+
			"DecodeConfig and Decode, and any other error means it did not", err)
	}
	if !strings.Contains(err.Error(), "40000x40000") {
		t.Errorf("the error does not name the dimensions: %v", err)
	}
}

// Both failure modes are PERMANENT: the same bytes fail the same way forever, so
// the consumer must Term rather than redeliver.
func TestUndecodableBytesArePermanentlyUnpreviewable(t *testing.T) {
	for _, tt := range []struct {
		name string
		body []byte
	}{
		{"not an image at all", []byte("this is a text file")},
		{"empty", nil},
		{"a truncated png", pngOf(t, 64, 64)[:20]},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Generate(bytes.NewReader(tt.body))
			if !errors.Is(err, ErrNoPreview) {
				t.Fatalf("Generate = %v, want ErrNoPreview so the consumer terminates "+
					"rather than redelivering the same bytes five times", err)
			}
		})
	}
}

// SVG is an XML document that can carry script. "It is an image" is exactly the
// reasoning that gets it into an allowlist it does not belong in.
func TestDecodableExcludesSVG(t *testing.T) {
	if Decodable("image/svg+xml") {
		t.Error("SVG is decodable; it is scriptable XML and must never be rendered or previewed")
	}
	for _, ct := range []string{"image/jpeg", "image/png", "image/gif", "image/webp"} {
		if !Decodable(ct) {
			t.Errorf("%s is not decodable", ct)
		}
	}
	for _, ct := range []string{"application/pdf", "video/mp4", "text/plain", ""} {
		if Decodable(ct) {
			t.Errorf("%s reported decodable; the worker would fetch it and fail", ct)
		}
	}
}

// The extension and the media type are one fact. drive's presignThumb derives
// the served content type from the key's extension against a closed map, so a
// key written with a different suffix serves nothing.
func TestExtensionMatchesTheMediaType(t *testing.T) {
	if Extension != ".jpg" || MediaType != "image/jpeg" {
		t.Fatalf("Extension %q and MediaType %q disagree", Extension, MediaType)
	}
}
