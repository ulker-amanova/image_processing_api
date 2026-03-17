package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/disintegration/imaging"
	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	defaultPort            = "8080"
	defaultSourceLimit     = 50 << 20
	defaultOutputMaxW      = 1400
	defaultOutputMaxH      = 1400
	defaultCacheTTL        = 5 * time.Minute
	defaultPlaceholderSize = 256
	jwtDuration            = 24 * time.Hour
)

var (
	jwtSecret     = getEnv("JWT_SECRET", "please-change-me")
	maxSourceSize = mustParseInt(getEnv("MAX_SOURCE_SIZE", strconv.Itoa(defaultSourceLimit)))
	maxOutW       = mustParseInt(getEnv("MAX_OUTPUT_WIDTH", strconv.Itoa(defaultOutputMaxW)))
	maxOutH       = mustParseInt(getEnv("MAX_OUTPUT_HEIGHT", strconv.Itoa(defaultOutputMaxH)))
	cacheTTL      = mustParseDuration(getEnv("CACHE_TTL", defaultCacheTTL.String()))

	requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "image_processing_api_requests_total",
		Help: "Total API requests",
	}, []string{"endpoint", "status"})

	durationMetric = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "image_processing_api_request_duration_seconds",
		Help:    "HTTP request duration seconds",
		Buckets: prometheus.DefBuckets,
	}, []string{"endpoint"})

	cacheHit = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "image_processing_api_cache_hits_total",
		Help: "Cache hits",
	})
	cacheMiss = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "image_processing_api_cache_misses_total",
		Help: "Cache misses",
	})
)

type User struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"-"`
}

type ImageRecord struct {
	ID          string    `json:"id"`
	OwnerID     string    `json:"owner_id"`
	Filename    string    `json:"filename"`
	UploadedAt  time.Time `json:"uploaded_at"`
	ContentType string    `json:"content_type"`
	Width       int       `json:"width"`
	Height      int       `json:"height"`
	Data        []byte    `json:"-"`
}

type transformationRequest struct {
	Transformations transformationSpec `json:"transformations"`
}

type transformationSpec struct {
	Resize    *resizeSpec    `json:"resize,omitempty"`
	Crop      *cropSpec      `json:"crop,omitempty"`
	Rotate    *int           `json:"rotate,omitempty"`
	Format    string         `json:"format,omitempty"`
	Filters   *filterSpec    `json:"filters,omitempty"`
	Watermark *watermarkSpec `json:"watermark,omitempty"`
	Flip      string         `json:"flip,omitempty"`
	Mirror    bool           `json:"mirror,omitempty"`
}

type resizeSpec struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type cropSpec struct {
	Width  int `json:"width"`
	Height int `json:"height"`
	X      int `json:"x"`
	Y      int `json:"y"`
}

type filterSpec struct {
	Grayscale bool `json:"grayscale,omitempty"`
	Sepia     bool `json:"sepia,omitempty"`
}

type watermarkSpec struct {
	Text string `json:"text"`
}

var (
	userStore = make(map[string]*User)
	userMu    sync.RWMutex

	imageStore = make(map[string]*ImageRecord)
	imageMu    sync.RWMutex

	transformCache = newTransformCache(cacheTTL)
)

type cacheEntry struct {
	data       []byte
	lastAccess time.Time
}

type transformCacheService struct {
	mu    sync.RWMutex
	items map[string]*cacheEntry
	ttl   time.Duration
}

func newTransformCache(ttl time.Duration) *transformCacheService {
	c := &transformCacheService{items: map[string]*cacheEntry{}, ttl: ttl}
	go c.janitor()
	return c
}

func (c *transformCacheService) janitor() {
	t := time.NewTicker(1 * time.Minute)
	defer t.Stop()
	for range t.C {
		c.mu.Lock()
		for k, v := range c.items {
			if time.Since(v.lastAccess) > c.ttl {
				delete(c.items, k)
			}
		}
		c.mu.Unlock()
	}
}

func (c *transformCacheService) Get(k string) ([]byte, bool) {
	c.mu.RLock()
	item, ok := c.items[k]
	c.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Since(item.lastAccess) > c.ttl {
		c.mu.Lock()
		delete(c.items, k)
		c.mu.Unlock()
		return nil, false
	}
	c.mu.Lock()
	item.lastAccess = time.Now()
	c.mu.Unlock()
	return append([]byte(nil), item.data...), true
}

func (c *transformCacheService) Set(k string, data []byte) {
	c.mu.Lock()
	c.items[k] = &cacheEntry{data: append([]byte(nil), data...), lastAccess: time.Now()}
	c.mu.Unlock()
}

func main() {
	prometheus.MustRegister(requests, durationMetric, cacheHit, cacheMiss)

	r := mux.NewRouter()
	r.HandleFunc("/register", wrapHandler(registerHandler, "register")).Methods(http.MethodPost)
	r.HandleFunc("/login", wrapHandler(loginHandler, "login")).Methods(http.MethodPost)
	secured := r.PathPrefix("/").Subrouter()
	secured.Use(authMiddleware)
	secured.HandleFunc("/images", wrapHandler(uploadImageHandler, "uploadImage")).Methods(http.MethodPost)
	secured.HandleFunc("/images", wrapHandler(listImagesHandler, "listImages")).Methods(http.MethodGet)
	secured.HandleFunc("/images/{id}", wrapHandler(getImageHandler, "getImage")).Methods(http.MethodGet)
	secured.HandleFunc("/images/{id}/transform", wrapHandler(transformImageHandler, "transformImage")).Methods(http.MethodPost)

	r.HandleFunc("/health", healthHandler).Methods(http.MethodGet)
	r.HandleFunc("/ready", readyHandler).Methods(http.MethodGet)
	r.Handle("/metrics", promhttp.Handler())

	port := getEnv("PORT", defaultPort)
	log.Printf("starting HTTP server on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, r))
}

func wrapHandler(h func(http.ResponseWriter, *http.Request), name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h(w, r)
		durationMetric.WithLabelValues(name).Observe(time.Since(start).Seconds())
	}
}

func registerHandler(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		respondError(w, http.StatusBadRequest, err)
		return
	}
	if payload.Username == "" || payload.Password == "" {
		respondError(w, http.StatusBadRequest, errors.New("username and password required"))
		return
	}

	userMu.Lock()
	for _, u := range userStore {
		if u.Username == payload.Username {
			userMu.Unlock()
			respondError(w, http.StatusConflict, errors.New("username already taken"))
			return
		}
	}
	id := randomID(payload.Username)
	passwordHash := hashString(payload.Password)
	userStore[id] = &User{ID: id, Username: payload.Username, PasswordHash: passwordHash}
	userMu.Unlock()

	token, err := issueToken(id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{"user": userStore[id], "token": token})
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		respondError(w, http.StatusBadRequest, err)
		return
	}

	userMu.RLock()
	var found *User
	for _, u := range userStore {
		if u.Username == payload.Username {
			found = u
			break
		}
	}
	userMu.RUnlock()

	if found == nil || found.PasswordHash != hashString(payload.Password) {
		respondError(w, http.StatusUnauthorized, errors.New("invalid credentials"))
		return
	}

	token, err := issueToken(found.ID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"user": found, "token": token})
}

func uploadImageHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value("userID").(string)
	if !ok {
		respondError(w, http.StatusUnauthorized, errors.New("unauthorized"))
		return
	}

	if err := r.ParseMultipartForm(int64(maxSourceSize)); err != nil {
		respondError(w, http.StatusBadRequest, err)
		return
	}
	file, header, err := r.FormFile("image")
	if err != nil {
		respondError(w, http.StatusBadRequest, err)
		return
	}
	defer file.Close()

	buf := bytes.Buffer{}
	if _, err := io.Copy(&buf, io.LimitReader(file, int64(maxSourceSize)+1)); err != nil {
		respondError(w, http.StatusInternalServerError, err)
		return
	}
	if buf.Len() > maxSourceSize {
		respondError(w, http.StatusRequestEntityTooLarge, errors.New("image size exceeds limit"))
		return
	}

	img, _, err := image.Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		respondError(w, http.StatusBadRequest, err)
		return
	}

	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(header.Filename))
	}
	if !strings.HasPrefix(contentType, "image/") {
		respondError(w, http.StatusUnsupportedMediaType, errors.New("not a supported image format"))
		return
	}

	id := randomID(header.Filename + time.Now().String())
	record := &ImageRecord{
		ID:          id,
		OwnerID:     userID,
		Filename:    header.Filename,
		UploadedAt:  time.Now(),
		ContentType: contentType,
		Width:       img.Bounds().Dx(),
		Height:      img.Bounds().Dy(),
		Data:        buf.Bytes(),
	}

	imageMu.Lock()
	imageStore[id] = record
	imageMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(record)
}

func getImageHandler(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	id := mux.Vars(r)["id"]

	imageMu.RLock()
	record, present := imageStore[id]
	imageMu.RUnlock()

	if !present {
		respondError(w, http.StatusNotFound, errors.New("image not found"))
		return
	}
	if record.OwnerID != userID {
		respondError(w, http.StatusForbidden, errors.New("forbidden"))
		return
	}

	format := strings.ToLower(r.URL.Query().Get("format"))
	if format == "" {
		format = "png"
	}

	cacheKey := fmt.Sprintf("%s:%s", id, format)
	if cached, ok := transformCache.Get(cacheKey); ok {
		cacheHit.Inc()
		writeImageResponse(w, format, cached)
		return
	}

	srcImg, _, err := image.Decode(bytes.NewReader(record.Data))
	if err != nil {
		respondError(w, http.StatusInternalServerError, err)
		return
	}

	processed, err := applyTransformations(srcImg, transformationSpec{Format: format})
	if err != nil {
		respondError(w, http.StatusInternalServerError, err)
		return
	}

	outBuf := bytes.Buffer{}
	switch format {
	case "png": err = png.Encode(&outBuf, processed)
	case "jpg", "jpeg": err = imaging.Encode(&outBuf, processed, imaging.JPEG)
	case "webp": err = imaging.Encode(&outBuf, processed, imaging.JPEG) // fallback
	default:
		respondError(w, http.StatusBadRequest, errors.New("unsupported format"))
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, err)
		return
	}

	transformCache.Set(cacheKey, outBuf.Bytes())
	cacheMiss.Inc()
	writeImageResponse(w, format, outBuf.Bytes())
}

func listImagesHandler(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	page := mustParseInt(getQuery(r, "page", "1"))
	limit := mustParseInt(getQuery(r, "limit", "10"))
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 10
	}

	imageMu.RLock()
	images := make([]*ImageRecord, 0)
	for _, rec := range imageStore {
		if rec.OwnerID == userID {
			images = append(images, rec)
		}
	}
	imageMu.RUnlock()

	start := (page - 1) * limit
	end := start + limit
	if start > len(images) {
		start = len(images)
	}
	if end > len(images) {
		end = len(images)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"page": page, "limit": limit, "total": len(images), "items": images[start:end]})
}

func transformImageHandler(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	id := mux.Vars(r)["id"]

	imageMu.RLock()
	record, present := imageStore[id]
	imageMu.RUnlock()
	if !present {
		respondError(w, http.StatusNotFound, errors.New("image not found"))
		return
	}
	if record.OwnerID != userID {
		respondError(w, http.StatusForbidden, errors.New("forbidden"))
		return
	}

	var req transformationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, err)
		return
	}
	if req.Transformations.Format == "" {
		req.Transformations.Format = "png"
	}

	srcImg, _, err := image.Decode(bytes.NewReader(record.Data))
	if err != nil {
		respondError(w, http.StatusInternalServerError, err)
		return
	}

	outImg, err := applyTransformations(srcImg, req.Transformations)
	if err != nil {
		respondError(w, http.StatusBadRequest, err)
		return
	}

	if outImg.Bounds().Dx() > maxOutW || outImg.Bounds().Dy() > maxOutH {
		outImg = imaging.Resize(outImg, min(outImg.Bounds().Dx(), maxOutW), min(outImg.Bounds().Dy(), maxOutH), imaging.Lanczos)
	}

	outBuf := bytes.Buffer{}
	switch strings.ToLower(req.Transformations.Format) {
	case "png": err = png.Encode(&outBuf, outImg)
	case "jpeg", "jpg": err = imaging.Encode(&outBuf, outImg, imaging.JPEG)
	case "webp": err = imaging.Encode(&outBuf, outImg, imaging.JPEG) // fallback
	default:
		respondError(w, http.StatusBadRequest, errors.New("unsupported format"))
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, err)
		return
	}

	newID := randomID(id + time.Now().String())
	newRecord := &ImageRecord{
		ID:          newID,
		OwnerID:     userID,
		Filename:    record.Filename,
		UploadedAt:  time.Now(),
		ContentType: "image/" + strings.ToLower(req.Transformations.Format),
		Width:       outImg.Bounds().Dx(),
		Height:      outImg.Bounds().Dy(),
		Data:        outBuf.Bytes(),
	}

	imageMu.Lock()
	imageStore[newID] = newRecord
	imageMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(newRecord)
}

func applyTransformations(src image.Image, t transformationSpec) (image.Image, error) {
	out := src
	if t.Resize != nil {
		if t.Resize.Width <= 0 || t.Resize.Height <= 0 {
			return nil, errors.New("resize width and height must be positive")
		}
		out = imaging.Fill(out, t.Resize.Width, t.Resize.Height, imaging.Center, imaging.Lanczos)
	}
	if t.Crop != nil {
		rect := image.Rect(t.Crop.X, t.Crop.Y, t.Crop.X+t.Crop.Width, t.Crop.Y+t.Crop.Height)
		rect = rect.Intersect(out.Bounds())
		if rect.Empty() {
			return nil, errors.New("invalid crop rectangle")
		}
		out = imaging.Crop(out, rect)
	}
	if t.Rotate != nil {
		angle := *t.Rotate % 360
		switch angle {
		case 90, -270:
			out = imaging.Rotate90(out)
		case 180, -180:
			out = imaging.Rotate180(out)
		case 270, -90:
			out = imaging.Rotate270(out)
		case 0:
			// no-op
		default:
			out = imaging.Rotate(out, float64(angle), color.Transparent)
		}
	}
	if strings.ToLower(t.Flip) == "horizontal" {
		out = imaging.FlipH(out)
	} else if strings.ToLower(t.Flip) == "vertical" {
		out = imaging.FlipV(out)
	}
	if t.Mirror {
		out = imaging.FlipH(out)
	}
	if t.Filters != nil {
		if t.Filters.Grayscale {
			out = imaging.Grayscale(out)
		}
		if t.Filters.Sepia {
			out = imaging.AdjustSaturation(out, -100)
			out = imaging.AdjustContrast(out, 10)
			out = imaging.AdjustGamma(out, 0.9)
		}
	}
	if t.Watermark != nil && t.Watermark.Text != "" {
		wm := imaging.New(220, 30, color.NRGBA{255, 255, 255, 180})
		bw := out.Bounds().Dx() - wm.Bounds().Dx() - 10
		bh := out.Bounds().Dy() - wm.Bounds().Dy() - 10
		if bw < 0 { bw = 0 }
		if bh < 0 { bh = 0 }
		out = imaging.Overlay(out, wm, image.Pt(bw, bh), 1.0)
	}
	return out, nil
}

func getQuery(r *http.Request, key, def string) string {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	return v
}

func respondError(w http.ResponseWriter, status int, err error) {
	if status >= 400 && status < 600 {
		writeErrorPlaceholder(w, status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func writeErrorPlaceholder(w http.ResponseWriter, status int) {
	col := color.RGBA{255, 165, 0, 255}
	if status >= 500 {
		col = color.RGBA{220, 20, 60, 255}
	}
	img := imaging.New(defaultPlaceholderSize, defaultPlaceholderSize, col)
	w.Header().Set("Content-Type", "image/png")
	w.WriteHeader(status)
	_ = png.Encode(w, img)
}

func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		if authorization == "" || !strings.HasPrefix(authorization, "Bearer ") {
			respondError(w, http.StatusUnauthorized, errors.New("missing token"))
			return
		}
		tokenString := strings.TrimPrefix(authorization, "Bearer ")

		claims := jwt.MapClaims{}
		token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
			return []byte(jwtSecret), nil
		})
		if err != nil || !token.Valid {
			respondError(w, http.StatusUnauthorized, errors.New("invalid token"))
			return
		}

		userID, ok := claims["sub"].(string)
		if !ok || userID == "" {
			respondError(w, http.StatusUnauthorized, errors.New("invalid token claims"))
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), "userID", userID)))
	})
}

func issueToken(userID string) (string, error) {
	claims := jwt.MapClaims{}
	claims["sub"] = userID
	claims["exp"] = time.Now().Add(jwtDuration).Unix()

	jwtToken := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return jwtToken.SignedString([]byte(jwtSecret))
}

func randomID(source string) string {
	h := sha256.Sum256([]byte(source + time.Now().String()))
	return base64.RawURLEncoding.EncodeToString(h[:12])
}

func hashString(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:])
}

func getEnv(key, def string) string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v
}

func mustParseInt(s string) int {
	v, err := strconv.Atoi(s)
	if err != nil || v == 0 {
		return 0
	}
	return v
}

func mustParseDuration(s string) time.Duration {
	v, err := time.ParseDuration(s)
	if err != nil {
		return defaultCacheTTL
	}
	return v
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func writeImageResponse(w http.ResponseWriter, format string, payload []byte) {
	ct := "image/png"
	format = strings.ToLower(format)
	if format == "jpeg" || format == "jpg" {
		ct = "image/jpeg"
	} else if format == "webp" {
		ct = "image/webp"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	w.Write(payload)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func readyHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ready"))
}
