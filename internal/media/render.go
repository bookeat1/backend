package media

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/draw"

	"github.com/kolesa-team/go-webp/encoder"
	"github.com/kolesa-team/go-webp/webp"

	// Registered for their side effect: decoding a source we did not choose.
	// The bucket holds JPEG, PNG and a handful of WebP; GIF is here because
	// nothing stops an upload form from accepting one and a decoder we do not
	// register turns into "unknown format" at backfill time.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	// WebP DECODING of pre-existing originals. golang.org/x/image/webp is a
	// pure-Go decoder with no encoder of its own; kept here rather than
	// switched over to go-webp's (cgo) decoder because it already has this
	// package's test coverage and needs nothing beyond the Go toolchain to
	// decode with. Importing github.com/kolesa-team/go-webp/webp below (for
	// its Encode) also registers a second, libwebp-backed "webp" decoder as a
	// side effect of its own init() — harmless duplication, not a conflict:
	// image.Decode tries registered decoders until one succeeds, and both
	// decode the same bytes correctly.
	//
	// This import is not theoretical: a dry run over the live bucket on
	// 2026-07-27 reported exactly 6 "unsupported" results, which is the 3
	// .webp originals times the 2 sizes. Without it those three photos would
	// silently keep being served full size forever, and the skip log would
	// blame the file rather than the missing decoder.
	_ "golang.org/x/image/webp"
)

// Quality is the WebP encoder quality of every derivative, on libwebp's 0-100
// scale.
//
// 82, unchanged from this package's original JPEG encoder: it is the value
// the old web app's client-side compressor already uses
// (book-eat-app/src/lib/compressImage.ts), so a photo does not visibly change
// character depending on which path produced it. The two encoders' quality
// numbers are not the same metric, but at this end of the scale the practical
// effect is the same shape — above ~85 a photo of a plate of food gains bytes
// much faster than it gains detail; below ~75 flat areas such as a tablecloth
// start to show blocking/banding at exactly the sizes we are generating. If a
// future measurement pass wants to retune this for WebP specifically, that is
// a one-constant change with the same test fixtures already in place.
const Quality = 82

// preset is the libwebp encoding preset. Photo, not Default: every source
// this package resizes is a photograph (a restaurant, a dish, a story), never
// a screenshot, line drawing or icon, and WebP's photo preset tunes the
// segmentation/filtering heuristics for exactly that content instead of
// leaving them at a generic default.
const preset = encoder.PresetPhoto

// ErrTooSmall is returned when the source is already no wider than the
// requested derivative. It is an expected outcome, not a failure: some
// originals in the bucket are 400px wide and there is nothing to gain from a
// 640px copy of them.
//
// Callers must treat this as "skip, and record why" — never as "retry", and
// never by writing an upscaled copy. Upscaling would spend storage to make a
// file BIGGER than the original it is supposed to replace, which is the exact
// opposite of the point.
var ErrTooSmall = errors.New("media: source is not wider than the requested derivative")

// ErrUnsupported wraps a source we could not decode at all.
var ErrUnsupported = errors.New("media: unsupported or corrupt image")

// Rendered is one generated derivative, ready to be written to the bucket.
type Rendered struct {
	Bytes       []byte
	ContentType string
	Width       int
	Height      int
}

// Render decodes src and returns a WebP scaled to exactly `width` pixels
// wide, preserving the aspect ratio.
//
// It never upscales: a source narrower than or equal to `width` returns
// ErrTooSmall and no bytes.
//
// TRANSPARENCY. The source is decoded into whatever colour model it carries
// and drawn straight into a premultiplied-alpha RGBA canvas — no background
// flattening. That is a deliberate change from this package's original JPEG
// output: JPEG has no alpha channel, so a transparent pixel encoded straight
// to JPEG came out BLACK, and the fix at the time was to flatten every source
// onto an opaque white canvas before encoding (right for the 47 PNGs in the
// bucket, all photos on a white-or-near-white app background, but lossy for
// any future logo that does use its alpha). WebP encodes alpha natively, so
// there is no longer a reason to throw it away: a transparent PNG now stays
// transparent through to the derivative. See
// TestRenderPreservesTransparencyInWebP.
func Render(src []byte, width int) (Rendered, error) {
	if width <= 0 {
		return Rendered{}, fmt.Errorf("media: bad target width %d", width)
	}

	img, format, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		return Rendered{}, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	_ = format // kept for readability of the decode step; not part of the output contract

	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return Rendered{}, fmt.Errorf("%w: zero-sized image", ErrUnsupported)
	}
	if sw <= width {
		return Rendered{}, fmt.Errorf("%w: source %dpx wide, target %dpx", ErrTooSmall, sw, width)
	}

	// Round the height rather than truncating it, and never let it reach 0 on
	// an extreme panorama.
	height := (sh*width + sw/2) / sw
	if height < 1 {
		height = 1
	}

	// draw.Src, not draw.Over: there is no background to composite onto
	// anymore, just a format conversion into a premultiplied-alpha canvas
	// that the box filter below can average correctly (premultiplied values
	// average correctly per channel; straight/non-premultiplied ones would
	// bleed the colour of fully transparent pixels into a translucent edge).
	flat := image.NewRGBA(b)
	draw.Draw(flat, b, img, b.Min, draw.Src)

	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	scale(dst, flat)

	opts, err := encoder.NewLossyEncoderOptions(preset, Quality)
	if err != nil {
		return Rendered{}, fmt.Errorf("media: encoder options: %w", err)
	}

	var out bytes.Buffer
	// Pre-size the buffer to something in the right order of magnitude so the
	// encoder does not walk a doubling ladder for every one of 772 objects.
	out.Grow(width * height / 8)
	// webp.Encode accepts dst (an *image.RGBA, premultiplied) directly: its
	// encoder converts to the *image.NRGBA libwebp wants via the standard
	// image/color machinery, which unpremultiplies correctly along the way.
	if err := webp.Encode(&out, dst, opts); err != nil {
		return Rendered{}, fmt.Errorf("media: encode: %w", err)
	}

	return Rendered{
		Bytes:       out.Bytes(),
		ContentType: "image/webp",
		Width:       width,
		Height:      height,
	}, nil
}

// scale writes a downscaled copy of src into dst.
//
// It is a box filter — every destination pixel is the unweighted average of
// the source pixels that map onto it — implemented here rather than pulled in
// from golang.org/x/image/draw.
//
// WHY NOT x/image/draw.CatmullRom. It is better at this and it is the obvious
// choice; the reason to not take it is that this is the only image work in the
// entire service, it runs in a one-off batch command rather than in a request,
// and a box filter over a 4x-or-more downscale is visually very close to a
// proper kernel because it is already averaging 16+ source pixels per output
// pixel. What a box filter is genuinely bad at is SMALL reductions (a 1.1x
// downscale aliases badly) — and at the ratios here (7.79 MB originals are
// 3000-4000px wide going to 640) that regime never occurs. If a future upload
// path needs a 1.2x resize in a request handler, take the dependency then;
// this stays honest about being a batch tool.
//
// Averaging happens in a uint32 accumulator per channel, so a source region of
// up to ~16 million pixels cannot overflow.
//
// ALPHA is averaged the same way as colour, not forced opaque. src carries
// PREMULTIPLIED alpha (image.RGBA's native form, and what draw.Draw produced
// it as in Render), so a box average of the raw bytes is the correct way to
// downsample it: a destination pixel straddling an opaque and a fully
// transparent source pixel comes out both half-as-bright and half-opaque,
// which is what "half opaque, half see-through" should look like. Averaging
// NON-premultiplied (straight) alpha the same naive way would instead bleed
// whatever colour a fully-transparent source pixel happens to hold into a
// translucent edge — a classic "black halo around a PNG cutout" bug.
func scale(dst *image.RGBA, src *image.RGBA) {
	db := dst.Bounds()
	sb := src.Bounds()
	dw, dh := db.Dx(), db.Dy()
	sw, sh := sb.Dx(), sb.Dy()

	for dy := 0; dy < dh; dy++ {
		// Source rows [y0,y1) that this destination row covers.
		y0 := sb.Min.Y + dy*sh/dh
		y1 := sb.Min.Y + (dy+1)*sh/dh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for dx := 0; dx < dw; dx++ {
			x0 := sb.Min.X + dx*sw/dw
			x1 := sb.Min.X + (dx+1)*sw/dw
			if x1 <= x0 {
				x1 = x0 + 1
			}

			var r, g, b, a, n uint32
			for y := y0; y < y1; y++ {
				row := src.PixOffset(x0, y)
				for x := x0; x < x1; x++ {
					r += uint32(src.Pix[row])
					g += uint32(src.Pix[row+1])
					b += uint32(src.Pix[row+2])
					a += uint32(src.Pix[row+3])
					n++
					row += 4
				}
			}
			// n is never 0: the clamps above guarantee at least one source
			// pixel per destination pixel.
			o := dst.PixOffset(db.Min.X+dx, db.Min.Y+dy)
			dst.Pix[o] = uint8(r / n)
			dst.Pix[o+1] = uint8(g / n)
			dst.Pix[o+2] = uint8(b / n)
			dst.Pix[o+3] = uint8(a / n)
		}
	}
}
