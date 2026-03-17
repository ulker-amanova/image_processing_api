package image

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"crypto/sha256"

	"github.com/disintegration/imaging"
)

type Record struct {
	ID          string    `json:"id"`
	OwnerID     string    `json:"owner_id"`
	Filename    string    `json:"filename"`
	UploadedAt  time.Time `json:"uploaded_at"`
	ContentType string    `json:"content_type"`
	Width       int       `json:"width"`
	Height      int       `json:"height"`
	Data        []byte    `json:"-"`
}

type Store struct {
	mu     sync.RWMutex
	images map[string]*Record
}

func NewStore() *Store {
	return &Store{images: map[string]*Record{}}
}

func (s *Store) Save(r *Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.images[r.ID] = r
}

func (s *Store) Load(id string) (*Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.images[id]
	return r, ok
}

func (s *Store) ListByUser(userID string) []*Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Record, 0)
	for _, r := range s.images {
		if r.OwnerID == userID {
			out = append(out, r)
		}
	}
	return out
}

type TransformOps struct {
	Resize  *SizeStruct `json:"resize,omitempty"`
	Crop    *CropStruct `json:"crop,omitempty"`
	Rotate  *int        `json:"rotate,omitempty"`
	Format  string      `json:"format,omitempty"`
	Filters *struct {
		Grayscale bool `json:"grayscale,omitempty"`
		Sepia     bool `json:"sepia,omitempty"`
	} `json:"filters,omitempty"`
	Flip   string `json:"flip,omitempty"`
	Mirror bool   `json:"mirror,omitempty"`
	Watermark *struct {
		Text string `json:"text,omitempty"`
	} `json:"watermark,omitempty"`
}

type SizeStruct struct { Width int `json:"width"`; Height int `json:"height"` }

type CropStruct struct { Width int `json:"width"`; Height int `json:"height"`; X int `json:"x"`; Y int `json:"y"` }

func NormalizeFormat(format string) string {
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "jpg" {
		format = "jpeg"
	}
	if format == "" {
		format = "png"
	}
	return format
}

func EncodeImage(img image.Image, format string) ([]byte, error) {
	buf := bytes.Buffer{}
	switch format {
	case "png":
		err := png.Encode(&buf, img)
		return buf.Bytes(), err
	case "jpeg":
		err := imaging.Encode(&buf, img, imaging.JPEG)
		return buf.Bytes(), err
	case "webp":
		// fallback to jpeg for WebP in this build (imaging WebP requires extra packages)
		err := imaging.Encode(&buf, img, imaging.JPEG)
		return buf.Bytes(), err
	default:
		return nil, errors.New("unsupported format")
	}
}

func TransformImage(img image.Image, ops TransformOps) (image.Image, error) {
	out := img
	if ops.Resize != nil {
		if ops.Resize.Width <= 0 || ops.Resize.Height <= 0 {
			return nil, errors.New("invalid resize values")
		}
		out = imaging.Fill(out, ops.Resize.Width, ops.Resize.Height, imaging.Center, imaging.Lanczos)
	}
	if ops.Crop != nil {
		rect := image.Rect(ops.Crop.X, ops.Crop.Y, ops.Crop.X+ops.Crop.Width, ops.Crop.Y+ops.Crop.Height)
		rect = rect.Intersect(out.Bounds())
		if rect.Empty() {
			return nil, errors.New("crop region invalid")
		}
		out = imaging.Crop(out, rect)
	}
	if ops.Rotate != nil {
		angle := *ops.Rotate % 360
		switch angle {
		case 90, -270:
			out = imaging.Rotate90(out)
		case 180:
			out = imaging.Rotate180(out)
		case 270, -90:
			out = imaging.Rotate270(out)
		case 0:
		default:
			out = imaging.Rotate(out, float64(angle), color.Transparent)
		}
	}
	if strings.EqualFold(ops.Flip, "horizontal") {
		out = imaging.FlipH(out)
	} else if strings.EqualFold(ops.Flip, "vertical") {
		out = imaging.FlipV(out)
	}
	if ops.Mirror {
		out = imaging.FlipH(out)
	}
	if ops.Filters != nil {
		if ops.Filters.Grayscale {
			out = imaging.Grayscale(out)
		}
		if ops.Filters.Sepia {
			out = imaging.AdjustSaturation(out, -100)
			out = imaging.AdjustContrast(out, 10)
			out = imaging.AdjustGamma(out, 0.9)
		}
	}
	return out, nil
}

func ParseUpload(req *http.Request, maxSourceSize int) (image.Image, []byte, string, error) {
	if err := req.ParseMultipartForm(int64(maxSourceSize)); err != nil {
		return nil, nil, "", err
	}
	file, header, err := req.FormFile("image")
	if err != nil {
		return nil, nil, "", err
	}
	defer file.Close()

	buf := bytes.Buffer{}
	if _, err := io.Copy(&buf, io.LimitReader(file, int64(maxSourceSize)+1)); err != nil {
		return nil, nil, "", err
	}
	if buf.Len() > maxSourceSize {
		return nil, nil, "", errors.New("image too large")
	}

	img, _, err := image.Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		return nil, nil, "", err
	}

	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(header.Filename))
	}
	return img, buf.Bytes(), contentType, nil
}

func ToID(str string) string {
	h := sha256.Sum256([]byte(str + time.Now().String()))
	return base64.RawURLEncoding.EncodeToString(h[:12])
}

type TransformCache struct {
	mu    sync.RWMutex
	items map[string]*cacheEntry
	ttl   time.Duration
}

type cacheEntry struct {
	data       []byte
	lastAccess time.Time
}

func NewTransformCache(ttl time.Duration) *TransformCache {
	c := &TransformCache{items: map[string]*cacheEntry{}, ttl: ttl}
	go c.janitor()
	return c
}

func (c *TransformCache) Get(key string) ([]byte, bool) {
	c.mu.RLock()
	item, ok := c.items[key]
	c.mu.RUnlock()
	if !ok || time.Since(item.lastAccess) > c.ttl {
		if ok {
			c.mu.Lock(); delete(c.items, key); c.mu.Unlock()
		}
		return nil, false
	}
	c.mu.Lock(); item.lastAccess = time.Now(); c.mu.Unlock()
	return append([]byte(nil), item.data...), true
}

func (c *TransformCache) Set(key string, data []byte) {
	c.mu.Lock()
	c.items[key] = &cacheEntry{data: append([]byte(nil), data...), lastAccess: time.Now()}
	c.mu.Unlock()
}

func (c *TransformCache) janitor() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		c.mu.Lock()
		for k, v := range c.items {
			if time.Since(v.lastAccess) > c.ttl {
				delete(c.items, k)
			}
		}
		c.mu.Unlock()
	}
}

func ToTransformResponse(r *Record) map[string]interface{} {
	return map[string]interface{}{"id": r.ID, "url": "/images/" + r.ID, "width": r.Width, "height": r.Height, "content_type": r.ContentType}
}
