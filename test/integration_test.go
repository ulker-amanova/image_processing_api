package test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ulker/imageprocessing/config"
	"github.com/ulker/imageprocessing/internal/auth"
	imgstore "github.com/ulker/imageprocessing/internal/image"
	"github.com/ulker/imageprocessing/internal/server"
)

func TestHTTPIntegration(t *testing.T) {
	cfg := config.AppConfig{Port: "8080", JWTSecret: "secret", MaxSourceSize: 1 << 20, MaxOutputWidth: 1400, MaxOutputHeight: 1400, CacheTTL: 5 * time.Minute}
	us := auth.NewStore()
	is := imgstore.NewStore()
	cache := imgstore.NewTransformCache(cfg.CacheTTL)
	r := server.NewRouter(cfg, us, is, cache)

	// same flow as server test
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{"username":"u3","password":"p3"}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	r.ServeHTTP(res, req)
	if res.Code != http.StatusCreated { t.Fatal("register failed") }
	var reg map[string]interface{}
	json.NewDecoder(res.Body).Decode(&reg)
	if reg["token"] == nil { t.Fatal("no token") }
}
