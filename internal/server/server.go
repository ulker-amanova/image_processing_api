package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdimage "image"
	"image/color"
	"image/png"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/disintegration/imaging"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/ulker/imageprocessing/config"
	"github.com/ulker/imageprocessing/internal/auth"
	imgstore "github.com/ulker/imageprocessing/internal/image"
)

const placeholderSize = 256

var (
	requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "image_processing_api_requests_total",
		Help: "Total API requests",
	}, []string{"endpoint", "status"})

	durationMetric = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "image_processing_api_request_duration_seconds",
		Help:    "HTTP request duration",
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

	rateLimitDenied = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "image_processing_api_rate_limited_total",
		Help: "Total rate-limited requests",
	})
	activeTransformWorkers = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "image_processing_api_active_transform_workers",
		Help: "Number of active transform workers",
	})

	rateLimiterChan chan struct{}
	registerOnce    sync.Once
)

func initRateLimiter(cfg config.AppConfig) {
	rateLimiterChan = make(chan struct{}, cfg.RateLimitPerSec)
	for i := 0; i < cfg.RateLimitPerSec; i++ {
		rateLimiterChan <- struct{}{}
	}
	ticker := time.NewTicker(time.Second)
	go func() {
		for range ticker.C {
			select {
			case rateLimiterChan <- struct{}{}:
			default:
			}
		}
	}()
}

func rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-rateLimiterChan:
			// token obtained; proceed
		default:
			rateLimitDenied.Inc()
			respondErrorPNG(w, http.StatusTooManyRequests, errors.New("rate limit exceeded"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func NewRouter(cfg config.AppConfig, us *auth.Store, is *imgstore.Store, transCache *imgstore.TransformCache) *mux.Router {
	registerOnce.Do(func() {
		prometheus.MustRegister(requests, durationMetric, cacheHit, cacheMiss, rateLimitDenied, activeTransformWorkers)
	})

	if cfg.RateLimitPerSec <= 0 {
		cfg.RateLimitPerSec = 20
	}
	initRateLimiter(cfg)

	r := mux.NewRouter()
	r.Use(rateLimitMiddleware)
	r.HandleFunc("/register", metricsHandler(registerHandler(us, cfg), "register")).Methods(http.MethodPost)
	r.HandleFunc("/login", metricsHandler(loginHandler(us, cfg), "login")).Methods(http.MethodPost)

	authed := r.NewRoute().Subrouter()
	authed.Use(authMiddleware(cfg.JWTSecret))
	authed.HandleFunc("/images", metricsHandler(uploadImageHandler(is, cfg), "uploadImage")).Methods(http.MethodPost)
	authed.HandleFunc("/images", metricsHandler(listImagesHandler(is), "listImages")).Methods(http.MethodGet)
	authed.HandleFunc("/images/{id}", metricsHandler(getImageHandler(is, transCache, cfg), "getImage")).Methods(http.MethodGet)
	authed.HandleFunc("/images/{id}/transform", metricsHandler(transformImageHandler(is, transCache, cfg), "transformImage")).Methods(http.MethodPost)

	r.HandleFunc("/health", healthHandler).Methods(http.MethodGet)
	r.HandleFunc("/ready", readyHandler).Methods(http.MethodGet)
	r.Handle("/metrics", promhttp.Handler())

	return r
}

func authMiddleware(secret string) mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if !strings.HasPrefix(authHeader, "Bearer ") {
				respondErrorPNG(w, http.StatusUnauthorized, errors.New("unauthorized"))
				return
			}
			userID, err := auth.ParseToken(secret, strings.TrimPrefix(authHeader, "Bearer "))
			if err != nil {
				respondErrorPNG(w, http.StatusUnauthorized, errors.New("unauthorized"))
				return
			}
			ctx := context.WithValue(r.Context(), "userID", userID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func metricsHandler(fn http.HandlerFunc, name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		fn(w, r)
		durationMetric.WithLabelValues(name).Observe(time.Since(start).Seconds())
	}
}

func registerHandler(us *auth.Store, cfg config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondErrorPNG(w, http.StatusBadRequest, err)
			return
		}
		if req.Username == "" || req.Password == "" {
			respondErrorPNG(w, http.StatusBadRequest, errors.New("username/password required"))
			return
		}
		user, err := us.Register(req.Username, req.Password)
		if err != nil { respondErrorPNG(w, http.StatusConflict, err); return }
		token, err := auth.CreateToken(cfg.JWTSecret, user.ID, 24*time.Hour)
		if err != nil { respondErrorPNG(w, http.StatusInternalServerError, err); return }
		writeJSON(w, http.StatusCreated, map[string]interface{}{"user": user, "token": token})
	}
}

func loginHandler(us *auth.Store, cfg config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondErrorPNG(w, http.StatusBadRequest, err)
			return
		}
		user, err := us.Authenticate(req.Username, req.Password)
		if err != nil { respondErrorPNG(w, http.StatusUnauthorized, err); return }
		token, err := auth.CreateToken(cfg.JWTSecret, user.ID, 24*time.Hour)
		if err != nil { respondErrorPNG(w, http.StatusInternalServerError, err); return }
		writeJSON(w, http.StatusOK, map[string]interface{}{"user": user, "token": token})
	}
}

func uploadImageHandler(is *imgstore.Store, cfg config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.Context().Value("userID").(string)
		img, raw, contentType, err := imgstore.ParseUpload(r, cfg.MaxSourceSize)
		if err != nil { respondErrorPNG(w, http.StatusBadRequest, err); return }
		bounds := img.Bounds()
		record := &imgstore.Record{ID: imgstore.ToID(contentType + time.Now().String()), OwnerID: userID, Filename: filepath.Base(r.FormValue("filename")), UploadedAt: time.Now(), ContentType: contentType, Width: bounds.Dx(), Height: bounds.Dy(), Data: raw}
		is.Save(record)
		writeJSON(w, http.StatusCreated, record)
	}
}

func listImagesHandler(is *imgstore.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.Context().Value("userID").(string)
		items := is.ListByUser(userID)
		page := parseIntQuery(r, "page", 1)
		limit := parseIntQuery(r, "limit", 10)
		start := (page - 1) * limit
		end := start + limit
		if start > len(items) { start = len(items) }
		if end > len(items) { end = len(items) }
		writeJSON(w, http.StatusOK, map[string]interface{}{"page": page, "limit": limit, "total": len(items), "items": items[start:end]})
	}
}

func getImageHandler(is *imgstore.Store, cache *imgstore.TransformCache, cfg config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.Context().Value("userID").(string)
		id := mux.Vars(r)["id"]
		record, ok := is.Load(id)
		if !ok { respondErrorPNG(w, http.StatusNotFound, errors.New("not found")); return }
		if record.OwnerID != userID { respondErrorPNG(w, http.StatusForbidden, errors.New("forbidden")); return }
		format := imgstore.NormalizeFormat(r.URL.Query().Get("format"))
		cacheKey := id + ":" + format
		if data, ok := cache.Get(cacheKey); ok { cacheHit.Inc(); writeImage(w, format, data); return }
		img, _, err := stdimage.Decode(bytes.NewReader(record.Data))
		if err != nil { respondErrorPNG(w, http.StatusInternalServerError, err); return }
		if img.Bounds().Dx() > cfg.MaxOutputWidth || img.Bounds().Dy() > cfg.MaxOutputHeight {
			img = imaging.Resize(img, min(img.Bounds().Dx(), cfg.MaxOutputWidth), min(img.Bounds().Dy(), cfg.MaxOutputHeight), imaging.Lanczos)
		}
		out, err := imgstore.EncodeImage(img, format)
		if err != nil { respondErrorPNG(w, http.StatusBadRequest, err); return }
		cache.Set(cacheKey, out)
		cacheMiss.Inc()
		writeImage(w, format, out)
	}
}

func transformImageHandler(is *imgstore.Store, cache *imgstore.TransformCache, cfg config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.Context().Value("userID").(string)
		id := mux.Vars(r)["id"]
		record, ok := is.Load(id)
		if !ok { respondErrorPNG(w, http.StatusNotFound, errors.New("not found")); return }
		if record.OwnerID != userID { respondErrorPNG(w, http.StatusForbidden, errors.New("forbidden")); return }

		var req struct {
			Transformations imgstore.TransformOps `json:"transformations"`
			ParallelResizes []imgstore.SizeStruct `json:"parallel_resizes,omitempty"`
			MaxWorkers      int                  `json:"max_workers,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil { respondErrorPNG(w, http.StatusBadRequest, err); return }

		img, _, err := stdimage.Decode(bytes.NewReader(record.Data))
		if err != nil { respondErrorPNG(w, http.StatusInternalServerError, err); return }

		outImg, err := imgstore.TransformImage(img, req.Transformations)
		if err != nil { respondErrorPNG(w, http.StatusBadRequest, err); return }
		if outImg.Bounds().Dx() > cfg.MaxOutputWidth || outImg.Bounds().Dy() > cfg.MaxOutputHeight {
			outImg = imaging.Resize(outImg, min(outImg.Bounds().Dx(), cfg.MaxOutputWidth), min(outImg.Bounds().Dy(), cfg.MaxOutputHeight), imaging.Lanczos)
		}

		format := imgstore.NormalizeFormat(req.Transformations.Format)

		if len(req.ParallelResizes) > 0 {
			maxW := req.MaxWorkers
			if maxW <= 0 { maxW = 4 }
			activeTransformWorkers.Set(float64(maxW))
			defer activeTransformWorkers.Set(0)

			variants, err := imgstore.ResizeImagesParallel(outImg, req.ParallelResizes, format, maxW)
			if err != nil { respondErrorPNG(w, http.StatusBadRequest, err); return }

			outRecords := map[string]*imgstore.Record{}
			for _, size := range req.ParallelResizes {
				key := fmt.Sprintf("%dx%d", size.Width, size.Height)
				data, exists := variants[key]
				if !exists {
					continue
				}
				uid := imgstore.ToID(record.ID + ":" + key + time.Now().String())
				newRec := &imgstore.Record{ID: uid, OwnerID: userID, Filename: record.Filename, UploadedAt: time.Now(), ContentType: "image/" + format, Width: size.Width, Height: size.Height, Data: data}
				is.Save(newRec)
				outRecords[key] = newRec
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{"base": outImg.Bounds(), "variants": outRecords})
			return
		}

		payload, err := imgstore.EncodeImage(outImg, format)
		if err != nil { respondErrorPNG(w, http.StatusInternalServerError, err); return }
		newID := imgstore.ToID(record.ID + time.Now().String())
		newRec := &imgstore.Record{ID: newID, OwnerID: userID, Filename: record.Filename, UploadedAt: time.Now(), ContentType: "image/" + format, Width: outImg.Bounds().Dx(), Height: outImg.Bounds().Dy(), Data: payload}
		is.Save(newRec)
		writeJSON(w, http.StatusOK, newRec)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK); w.Write([]byte("ok")) }

func readyHandler(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK); w.Write([]byte("ready")) }

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}

func writeImage(w http.ResponseWriter, format string, payload []byte) {
	ct := "image/png"
	if format == "jpeg" || format == "jpg" { ct = "image/jpeg" }
	if format == "webp" { ct = "image/webp" }
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	w.Write(payload)
}

func respondErrorPNG(w http.ResponseWriter, status int, err error) {
	col := color.RGBA{255, 165, 0, 255}
	if status >= 500 { col = color.RGBA{220,20,60,255} }
	img := imaging.New(placeholderSize, placeholderSize, col)
	w.Header().Set("Content-Type", "image/png")
	w.WriteHeader(status)
	png.Encode(w, img)
}

func parseIntQuery(r *http.Request, name string, def int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || v < 1 { return def }
	return v
}

func min(a,b int) int { if a<b { return a }; return b }
