// Package imageproc does minimal on-the-fly image resizing for previews.
//
// Scope: image/resize only — NOT the full cloud image-processing API. It
// decodes (jpeg/png/gif), resizes with bilinear sampling, and re-encodes as
// jpeg or png. Pure standard library, no external dependencies.
package imageproc

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif" // register GIF decoder (first frame is used)
	"image/jpeg"
	"image/png"
	"io"
	"math"
)

// Mode selects how the source is fitted into the requested w×h box.
type Mode string

const (
	Lfit  Mode = "lfit"  // scale to fit INSIDE w×h, preserve aspect (default)
	Fill  Mode = "fill"  // scale to COVER w×h, then center-crop to exactly w×h
	Fixed Mode = "fixed" // scale to exactly w×h, ignoring aspect ratio
)

// MaxPixels caps the decoded source size (decode-bomb guard).
const MaxPixels = 40_000_000

// MaxBytes caps the encoded input size we will buffer and decode.
const MaxBytes = 20 << 20

var (
	ErrTooManyPixels = errors.New("image dimensions exceed processing limit")
	ErrTooLarge      = errors.New("image exceeds the processing size limit")
)

// ResizeReader reads up to MaxBytes from r and resizes the image. size is the
// known content length (0 if unknown), used for an early reject. Returns
// ErrTooLarge if the input exceeds MaxBytes.
func ResizeReader(r io.Reader, size int64, w, h int, mode Mode) (out []byte, contentType string, err error) {
	if size > MaxBytes {
		return nil, "", ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > MaxBytes {
		return nil, "", ErrTooLarge
	}
	return Resize(data, w, h, mode)
}

// Resize decodes data, resizes per (w, h, mode) and re-encodes. w or h may be 0
// ("derive from the other, preserving aspect"); at least one must be > 0. The
// returned content type is image/png for png sources, otherwise image/jpeg.
func Resize(data []byte, w, h int, mode Mode) (out []byte, contentType string, err error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}
	if cfg.Width*cfg.Height > MaxPixels {
		return nil, "", ErrTooManyPixels
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}

	dst := transform(img, w, h, mode)

	var buf bytes.Buffer
	switch format {
	case "png":
		contentType = "image/png"
		err = png.Encode(&buf, dst)
	default: // jpeg / gif(first frame) / other → jpeg
		contentType = "image/jpeg"
		err = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85})
	}
	if err != nil {
		return nil, "", err
	}
	return buf.Bytes(), contentType, nil
}

func transform(src image.Image, w, h int, mode Mode) image.Image {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw == 0 || sh == 0 {
		return src
	}
	switch mode {
	case Fill:
		if w > 0 && h > 0 {
			scale := math.Max(float64(w)/float64(sw), float64(h)/float64(sh))
			resized := bilinear(src, iround(float64(sw)*scale), iround(float64(sh)*scale))
			rb := resized.Bounds()
			return cropCenter(resized, min(w, rb.Dx()), min(h, rb.Dy()))
		}
	case Fixed:
		if w > 0 && h > 0 {
			return bilinear(src, w, h)
		}
	}
	// Lfit (default), and the fallback when Fill/Fixed lack a dimension.
	tw, th := fitDims(sw, sh, w, h)
	return bilinear(src, tw, th)
}

// fitDims returns the largest w×h-bounded size that preserves aspect ratio. A
// zero dimension is treated as unconstrained.
func fitDims(sw, sh, w, h int) (int, int) {
	switch {
	case w > 0 && h > 0:
		s := math.Min(float64(w)/float64(sw), float64(h)/float64(sh))
		return iround(float64(sw) * s), iround(float64(sh) * s)
	case w > 0:
		return w, iround(float64(sh) * float64(w) / float64(sw))
	case h > 0:
		return iround(float64(sw) * float64(h) / float64(sh)), h
	default:
		return sw, sh
	}
}

// bilinear resizes src to dw×dh using bilinear sampling.
func bilinear(src image.Image, dw, dh int) *image.RGBA {
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		fy := (float64(y)+0.5)*float64(sh)/float64(dh) - 0.5
		y0 := int(math.Floor(fy))
		ty := fy - float64(y0)
		for x := 0; x < dw; x++ {
			fx := (float64(x)+0.5)*float64(sw)/float64(dw) - 0.5
			x0 := int(math.Floor(fx))
			tx := fx - float64(x0)
			c00 := sample(src, b, x0, y0)
			c10 := sample(src, b, x0+1, y0)
			c01 := sample(src, b, x0, y0+1)
			c11 := sample(src, b, x0+1, y0+1)
			dst.SetRGBA(x, y, color.RGBA{
				R: lerp2(c00.R, c10.R, c01.R, c11.R, tx, ty),
				G: lerp2(c00.G, c10.G, c01.G, c11.G, tx, ty),
				B: lerp2(c00.B, c10.B, c01.B, c11.B, tx, ty),
				A: lerp2(c00.A, c10.A, c01.A, c11.A, tx, ty),
			})
		}
	}
	return dst
}

func sample(src image.Image, b image.Rectangle, x, y int) color.RGBA {
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x > b.Dx()-1 {
		x = b.Dx() - 1
	}
	if y > b.Dy()-1 {
		y = b.Dy() - 1
	}
	return color.RGBAModel.Convert(src.At(b.Min.X+x, b.Min.Y+y)).(color.RGBA)
}

func lerp2(c00, c10, c01, c11 uint8, tx, ty float64) uint8 {
	top := float64(c00)*(1-tx) + float64(c10)*tx
	bot := float64(c01)*(1-tx) + float64(c11)*tx
	v := top*(1-ty) + bot*ty
	if v < 0 {
		v = 0
	}
	if v > 255 {
		v = 255
	}
	return uint8(v + 0.5)
}

func cropCenter(img *image.RGBA, w, h int) image.Image {
	bx := (img.Bounds().Dx() - w) / 2
	by := (img.Bounds().Dy() - h) / 2
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), img, image.Pt(img.Bounds().Min.X+bx, img.Bounds().Min.Y+by), draw.Src)
	return dst
}

func iround(f float64) int { return int(f + 0.5) }
