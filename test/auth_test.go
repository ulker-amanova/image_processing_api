package test

import (
	"testing"
	"time"

	"github.com/ulker/imageprocessing/internal/auth"
)

func TestRegisterAndAuthenticate(t *testing.T) {
	store := auth.NewStore()
	user, err := store.Register("user1", "pass1")
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}
	if user.Username != "user1" {
		t.Fatalf("unexpected username: %s", user.Username)
	}

	_, err = store.Authenticate("user1", "pass1")
	if err != nil {
		t.Fatalf("authenticate failed: %v", err)
	}

	_, err = store.Authenticate("user1", "wrong")
	if err == nil {
		t.Fatal("expected invalid credentials")
	}
}

func TestDuplicateUser(t *testing.T) {
	store := auth.NewStore()
	_, err := store.Register("user1", "pass1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Register("user1", "pass1")
	if err != auth.ErrUserExists {
		t.Fatalf("expected ErrUserExists got %v", err)
	}
}

func TestJWTRoundtrip(t *testing.T) {
	secret := "test-secret"
	userID := "uid-123"
	token, err := auth.CreateToken(secret, userID, 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := auth.ParseToken(secret, token)
	if err != nil {
		t.Fatal(err)
	}
	if parsed != userID {
		t.Fatalf("expected %s got %s", userID, parsed)
	}
}
