package test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	imgstore "github.com/ulker/imageprocessing/internal/image"
)

func TestNormalizeAndEncode(t *testing.T) {
	if imgstore.NormalizeFormat("JPG") != "jpeg" {
		t.Fatalf("expected jpeg")
	}
	if imgstore.NormalizeFormat("") != "png" {
		t.Fatalf("expected png")
	}

	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	for x := 0; x < 10; x++ {
		for y := 0; y < 10; y++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 100, 255})
		}
	}
	b, err := imgstore.EncodeImage(img, "png")
	if err != nil || len(b) == 0 {
		t.Fatalf("encode image failed: %v", err)
	}
}

func TestTransformImage(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 40, 40))
	for x := 0; x < 40; x++ {
		for y := 0; y < 40; y++ {
			img.Set(x, y, color.RGBA{byte(x), byte(y), 100, 255})
		}
	}
	rot := 90
	out, err := imgstore.TransformImage(img, imgstore.TransformOps{Rotate: &rot})
	if err != nil {
		t.Fatalf("rotation failed: %v", err)
	}
	if out.Bounds().Dx() != 40 || out.Bounds().Dy() != 40 {
		t.Fatalf("unexpected bounds %v", out.Bounds())
	}
}

func TestParseUpload(t *testing.T) {
	buf := bytes.Buffer{}
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("image", "test.png")
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 5, 5))
	if err := png.Encode(part, img); err != nil {
		t.Fatal(err)
	}
	w.Close()

	req := httptest.NewRequest(http.MethodPost, "/", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())

	imgDec, data, contentType, err := imgstore.ParseUpload(req, 10<<20)
	if err != nil {
		t.Fatalf("parse upload failed: %v", err)
	}
	if contentType == "" || len(data) == 0 || imgDec.Bounds().Dx() != 5 {
		t.Fatalf("unexpected parse upload result: contentType=%s len(d)=%d bounds=%v", contentType, len(data), imgDec.Bounds())
	}
}
