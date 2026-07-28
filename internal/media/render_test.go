package media

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// sourceJPEG builds a w×h photo-like JPEG: a smooth gradient, which is what a
// real photo's statistics look like far better than flat colour does (flat
// colour compresses to almost nothing and would make the size assertions
// meaningless).
func sourceJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8((x * 255) / w),
				G: uint8((y * 255) / h),
				B: uint8(((x + y) * 255) / (w + h)),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("fixture encode: %v", err)
	}
	return buf.Bytes()
}

func TestRenderProducesRequestedWidthAndKeepsAspectRatio(t *testing.T) {
	src := sourceJPEG(t, 3000, 2000)

	got, err := Render(src, WidthSmall)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got.Width != WidthSmall {
		t.Fatalf("width = %d, want %d", got.Width, WidthSmall)
	}
	// 3000x2000 is 3:2, so 640 wide must be 427 high (2000*640/3000 = 426.67).
	if got.Height != 427 {
		t.Fatalf("height = %d, want 427", got.Height)
	}
	if got.ContentType != "image/jpeg" {
		t.Fatalf("content type = %q", got.ContentType)
	}

	cfg, format, err := image.DecodeConfig(bytes.NewReader(got.Bytes))
	if err != nil {
		t.Fatalf("output is not a decodable image: %v", err)
	}
	if format != "jpeg" {
		t.Fatalf("output format = %q, want jpeg", format)
	}
	if cfg.Width != got.Width || cfg.Height != got.Height {
		t.Fatalf("declared %dx%d, actual %dx%d", got.Width, got.Height, cfg.Width, cfg.Height)
	}
}

// The entire point of the feature. A derivative that is not dramatically
// smaller than its original is a bug, not a saving.
func TestRenderIsDramaticallySmallerThanTheOriginal(t *testing.T) {
	src := sourceJPEG(t, 3000, 2000)

	small, err := Render(src, WidthSmall)
	if err != nil {
		t.Fatalf("Render small: %v", err)
	}
	large, err := Render(src, WidthLarge)
	if err != nil {
		t.Fatalf("Render large: %v", err)
	}

	if len(small.Bytes) >= len(src)/5 {
		t.Fatalf("small derivative %d B is not <20%% of the %d B original", len(small.Bytes), len(src))
	}
	if len(large.Bytes) >= len(src) {
		t.Fatalf("large derivative %d B is not smaller than the %d B original", len(large.Bytes), len(src))
	}
	if len(small.Bytes) >= len(large.Bytes) {
		t.Fatalf("small (%d B) is not smaller than large (%d B)", len(small.Bytes), len(large.Bytes))
	}
	t.Logf("original %d B -> w640 %d B -> w1280 %d B", len(src), len(small.Bytes), len(large.Bytes))
}

// Upscaling would spend storage to make a file bigger than the original it is
// meant to replace.
func TestRenderRefusesToUpscale(t *testing.T) {
	src := sourceJPEG(t, 400, 300)

	if _, err := Render(src, WidthSmall); !errors.Is(err, ErrTooSmall) {
		t.Fatalf("Render of a 400px source at 640 = %v, want ErrTooSmall", err)
	}
	// Exactly equal width is also nothing to gain.
	if _, err := Render(sourceJPEG(t, WidthSmall, 480), WidthSmall); !errors.Is(err, ErrTooSmall) {
		t.Fatalf("Render of an exactly-640px source = %v, want ErrTooSmall", err)
	}
}

// A transparent PNG encoded straight to JPEG comes out with a BLACK
// background. The bucket holds 47 PNGs.
func TestRenderFlattensTransparencyOntoWhiteNotBlack(t *testing.T) {
	const w, h = 1600, 1200
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// Fully transparent everywhere: whatever colour comes out IS the background.
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("fixture encode: %v", err)
	}

	got, err := Render(buf.Bytes(), WidthSmall)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	out, err := jpeg.Decode(bytes.NewReader(got.Bytes))
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	r, g, b, _ := out.At(got.Width/2, got.Height/2).RGBA()
	// JPEG is lossy, so allow a little drift, but black (0) must be nowhere near.
	if r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
		t.Fatalf("transparent source flattened to rgb(%d,%d,%d), want near-white", r>>8, g>>8, b>>8)
	}
}

func TestRenderRejectsGarbage(t *testing.T) {
	if _, err := Render([]byte("this is not an image"), WidthSmall); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Render of junk = %v, want ErrUnsupported", err)
	}
	if _, err := Render(nil, WidthSmall); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Render of nil = %v, want ErrUnsupported", err)
	}
	if _, err := Render(sourceJPEG(t, 100, 100), 0); err == nil {
		t.Fatal("Render with width 0 succeeded, want error")
	}
}

// A panorama must not round its way to a zero-height image.
func TestRenderClampsExtremeAspectRatios(t *testing.T) {
	got, err := Render(sourceJPEG(t, 4000, 3), WidthSmall)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got.Height < 1 {
		t.Fatalf("height = %d, want at least 1", got.Height)
	}
}
