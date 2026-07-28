package media

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"

	// Registered for their side effect: decoding a source we did not choose.
	// The bucket holds JPEG, PNG and a handful of WebP; GIF is here because
	// nothing stops an upload form from accepting one and a decoder we do not
	// register turns into "unknown format" at backfill time.
	_ "image/gif"
	_ "image/png"

	// WebP DECODING only — golang.org/x/image/webp has no encoder, which is
	// fine because the output format here is JPEG. This import is not
	// theoretical: a dry run over the live bucket on 2026-07-27 reported
	// exactly 6 "unsupported" results, which is the 3 .webp originals times
	// the 2 sizes. Without it those three photos would silently keep being
	// served full size forever, and the skip log would blame the file rather
	// than the missing decoder.
	_ "golang.org/x/image/webp"
)

// Quality is the JPEG encoder quality of every derivative.
//
// 82, the same value the old web app's client-side compressor already uses
// (book-eat-app/src/lib/compressImage.ts), so a photo does not visibly change
// character depending on which path produced it. Above ~85 a photo of a plate
// of food gains bytes much faster than it gains detail; below ~75 flat areas
// such as a tablecloth start to show blocking at exactly the sizes we are
// generating.
const Quality = 82

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

// Render decodes src and returns a JPEG scaled to exactly `width` pixels wide,
// preserving the aspect ratio.
//
// It never upscales: a source narrower than or equal to `width` returns
// ErrTooSmall and no bytes.
//
// The source is decoded into whatever colour model it carries and then drawn
// onto an opaque WHITE canvas before scaling. That matters for the 47 PNGs in
// the bucket: JPEG has no alpha channel, so a transparent pixel encoded
// straight to JPEG comes out BLACK — a logo on a transparent background would
// turn into a logo in a black box. White is the right background because every
// surface these photos are drawn on in the app is white or near-white. (Twelve
// of those PNGs were sampled on 2026-07-27 and none actually used its alpha
// channel, so in practice this is belt and braces — but the next upload is not
// bound by that sample.)
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

	flat := image.NewRGBA(b)
	draw.Draw(flat, b, image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(flat, b, img, b.Min, draw.Over)

	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	scale(dst, flat)

	var out bytes.Buffer
	// Pre-size the buffer to something in the right order of magnitude so the
	// encoder does not walk a doubling ladder for every one of 772 objects.
	out.Grow(width * height / 8)
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: Quality}); err != nil {
		return Rendered{}, fmt.Errorf("media: encode: %w", err)
	}

	return Rendered{
		Bytes:       out.Bytes(),
		ContentType: "image/jpeg",
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

			var r, g, b, n uint32
			for y := y0; y < y1; y++ {
				row := src.PixOffset(x0, y)
				for x := x0; x < x1; x++ {
					r += uint32(src.Pix[row])
					g += uint32(src.Pix[row+1])
					b += uint32(src.Pix[row+2])
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
			// The source was flattened onto opaque white before scaling, so
			// every pixel is fully opaque by construction.
			dst.Pix[o+3] = 0xff
		}
	}
}
