package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrUserExists = errors.New("user exists")
)

type User struct {
	ID string `json:"id"`
	Username string `json:"username"`
	PasswordHash string `json:"-"`
}

type Store struct {
	mu sync.RWMutex
	users map[string]*User
}

func NewStore() *Store {
	return &Store{users: map[string]*User{}}
}

func (s *Store) Register(username, password string) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.users {
		if u.Username == username {
			return nil, ErrUserExists
		}
	}
	id := genID(username + time.Now().String())
	u := &User{ID: id, Username: username, PasswordHash: hash(password)}
	s.users[id] = u
	return u, nil
}

func (s *Store) Authenticate(username, password string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.users {
		if u.Username == username && u.PasswordHash == hash(password) {
			return u, nil
		}
	}
	return nil, ErrInvalidCredentials
}

func genID(source string) string {
	h := sha256.Sum256([]byte(source))
	return hex.EncodeToString(h[:8])
}

func hash(password string) string {
	h := sha256.Sum256([]byte(password))
	return hex.EncodeToString(h[:])
}

func CreateToken(secret, userID string, ttl time.Duration) (string, error) {
	claims := jwt.MapClaims{"sub": userID, "exp": time.Now().Add(ttl).Unix()}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return t.SignedString([]byte(secret))
}

func ParseToken(secret, tokenString string) (string, error) {
	claims := jwt.MapClaims{}
	okToken, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
		return []byte(secret), nil
	})
	if err != nil || !okToken.Valid {
		return "", err
	}
	sub, ok := claims["sub"].(string)
	if !ok || sub == "" {
		return "", errors.New("invalid token claims")
	}
	return sub, nil
}

func Middleware(secret string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		t, err := ParseToken(secret, strings.TrimPrefix(auth, "Bearer "))
		if err != nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), "userID", t)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
