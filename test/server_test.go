package test

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ulker/imageprocessing/config"
	"github.com/ulker/imageprocessing/internal/auth"
	"github.com/ulker/imageprocessing/internal/server"
	imgstore "github.com/ulker/imageprocessing/internal/image"
)

func TestFullWorkflow(t *testing.T) {
	cfg := config.AppConfig{Port: "8080", JWTSecret: "secret", MaxSourceSize: 1 << 20, MaxOutputWidth: 1400, MaxOutputHeight: 1400, CacheTTL: 5 * time.Minute}
	us := auth.NewStore()
	is := imgstore.NewStore()
	cache := imgstore.NewTransformCache(cfg.CacheTTL)
	r := server.NewRouter(cfg, us, is, cache)

	// register
	reg := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{"username":"u","password":"p"}`))
	reg.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	r.ServeHTTP(res, reg)
	if res.Code != http.StatusCreated {
		t.Fatalf("register expected 201 got %d", res.Code)
	}
	var regResp map[string]interface{}
	json.NewDecoder(res.Body).Decode(&regResp)
	if regResp["token"] == nil {
		t.Fatalf("register missing token")
	}

	// login
	login := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{"username":"u","password":"p"}`))
	login.Header.Set("Content-Type", "application/json")
	res = httptest.NewRecorder()
	r.ServeHTTP(res, login)
	if res.Code != http.StatusOK {
		t.Fatalf("login expected 200 got %d", res.Code)
	}
	var loginResp map[string]interface{}
	json.NewDecoder(res.Body).Decode(&loginResp)
	token := loginResp["token"].(string)

	// upload image
	uploadBody := &bytes.Buffer{}
	writer := multipart.NewWriter(uploadBody)
	part, err := writer.CreateFormFile("image", "pic.png")
	if err != nil { t.Fatal(err) }
	img := image.NewRGBA(image.Rect(0, 0, 10, 10));
	for x:=0; x<10; x++ { for y:=0; y<10; y++ { img.Set(x, y, color.RGBA{byte(x),byte(y),100,255}) }}
	if err := png.Encode(part, img); err != nil { t.Fatal(err) }
	writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/images", uploadBody)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	res = httptest.NewRecorder()
	r.ServeHTTP(res, req)
	if res.Code != http.StatusCreated { t.Fatalf("upload expected 201 got %d", res.Code) }

	var uploaded imgstore.Record
	json.NewDecoder(res.Body).Decode(&uploaded)
	if uploaded.ID == "" { t.Fatal("missing uploaded id") }

	// get image
	req = httptest.NewRequest(http.MethodGet, "/images/"+uploaded.ID+"?format=png", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res = httptest.NewRecorder()
	r.ServeHTTP(res, req)
	if res.Code != http.StatusOK { t.Fatalf("get expected 200 got %d", res.Code) }
	if res.Header().Get("Content-Type") != "image/png" { t.Fatalf("content type got %s", res.Header().Get("Content-Type")) }

	// list
	req = httptest.NewRequest(http.MethodGet, "/images?page=1&limit=10", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res = httptest.NewRecorder()
	r.ServeHTTP(res, req)
	if res.Code != http.StatusOK { t.Fatalf("list expected 200 got %d", res.Code) }
}
