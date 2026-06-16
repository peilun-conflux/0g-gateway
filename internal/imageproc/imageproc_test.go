package imageproc

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func dims(t *testing.T, data []byte) (int, int, string) {
	t.Helper()
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return cfg.Width, cfg.Height, format
}

func TestResizeLfitPreservesAspect(t *testing.T) {
	out, ct, err := Resize(makePNG(t, 100, 50), 20, 20, Lfit) // 2:1 into 20×20 box
	if err != nil {
		t.Fatal(err)
	}
	if ct != "image/png" {
		t.Fatalf("content type: %s", ct)
	}
	if w, h, _ := dims(t, out); w != 20 || h != 10 {
		t.Fatalf("lfit dims: %dx%d, want 20x10", w, h)
	}
}

func TestResizeSingleDimension(t *testing.T) {
	out, _, err := Resize(makePNG(t, 100, 50), 50, 0, Lfit) // width only
	if err != nil {
		t.Fatal(err)
	}
	if w, h, _ := dims(t, out); w != 50 || h != 25 {
		t.Fatalf("single-dim: %dx%d, want 50x25", w, h)
	}
}

func TestResizeFillCropsToExact(t *testing.T) {
	out, _, err := Resize(makePNG(t, 100, 50), 30, 30, Fill)
	if err != nil {
		t.Fatal(err)
	}
	if w, h, _ := dims(t, out); w != 30 || h != 30 {
		t.Fatalf("fill dims: %dx%d, want 30x30", w, h)
	}
}

func TestResizeFixedIgnoresAspect(t *testing.T) {
	out, _, err := Resize(makePNG(t, 100, 50), 33, 44, Fixed)
	if err != nil {
		t.Fatal(err)
	}
	if w, h, _ := dims(t, out); w != 33 || h != 44 {
		t.Fatalf("fixed dims: %dx%d, want 33x44", w, h)
	}
}

func TestResizeJPEGStaysJPEG(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 80, 80))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	_, ct, err := Resize(buf.Bytes(), 40, 40, Lfit)
	if err != nil {
		t.Fatal(err)
	}
	if ct != "image/jpeg" {
		t.Fatalf("jpeg content type: %s", ct)
	}
}

func TestResizeRejectsNonImage(t *testing.T) {
	if _, _, err := Resize([]byte("not an image at all"), 10, 10, Lfit); err == nil {
		t.Fatal("expected decode error for non-image input")
	}
}
