package test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ulker/imageprocessing/config"
	"github.com/ulker/imageprocessing/internal/auth"
	"github.com/ulker/imageprocessing/internal/image"
	"github.com/ulker/imageprocessing/internal/server"
)

func TestUnauthorizedReturnsPlaceholderPNG(t *testing.T) {
	cfg := config.AppConfig{Port: "8080", JWTSecret: "secret", MaxSourceSize: 1 << 20, MaxOutputWidth: 1400, MaxOutputHeight: 1400, CacheTTL: 5 * time.Minute}
	us := auth.NewStore()
	is := image.NewStore()
	cache := image.NewTransformCache(cfg.CacheTTL)
	r := server.NewRouter(cfg, us, is, cache)

	req := httptest.NewRequest(http.MethodGet, "/images/12345", nil)
	res := httptest.NewRecorder()
	r.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", res.Code)
	}
	if content := res.Header().Get("Content-Type"); content != "image/png" {
		t.Fatalf("expected image/png placeholder, got %q", content)
	}
}

func TestTransformNotFoundReturns404Placeholder(t *testing.T) {
	cfg := config.AppConfig{Port: "8080", JWTSecret: "secret", MaxSourceSize: 1 << 20, MaxOutputWidth: 1400, MaxOutputHeight: 1400, CacheTTL: 5 * time.Minute}
	us := auth.NewStore()
	is := image.NewStore()
	cache := image.NewTransformCache(cfg.CacheTTL)
	r := server.NewRouter(cfg, us, is, cache)

	user, _ := us.Register("u2", "p2")
	token, _ := auth.CreateToken(cfg.JWTSecret, user.ID, 5*time.Minute)

	req := httptest.NewRequest(http.MethodPost, "/images/doesnotexist/transform", strings.NewReader(`{"transformations":{"resize":{"width":100,"height":100}}}`))
	req.Header.Set("Authorization", "Bearer "+token)
	res := httptest.NewRecorder()
	r.ServeHTTP(res, req)
	if res.Code != http.StatusNotFound {
		t.Fatalf("expected 404 got %d", res.Code)
	}
	if content := res.Header().Get("Content-Type"); content != "image/png" {
		t.Fatalf("expected placeholder content-type image/png, got %s", content)
	}
}
