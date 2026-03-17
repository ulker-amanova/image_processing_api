package config

import (
	"os"
	"strconv"
	"time"
)

type AppConfig struct {
	Port           string
	JWTSecret      string
	MaxSourceSize  int
	MaxOutputWidth int
	MaxOutputHeight int
	CacheTTL       time.Duration
}

func Load() AppConfig {
	return AppConfig{
		Port:            getEnv("PORT", "8080"),
		JWTSecret:       getEnv("JWT_SECRET", "please-change-me"),
		MaxSourceSize:   mustParseInt(getEnv("MAX_SOURCE_SIZE", "52428800")),
		MaxOutputWidth:  mustParseInt(getEnv("MAX_OUTPUT_WIDTH", "1400")),
		MaxOutputHeight: mustParseInt(getEnv("MAX_OUTPUT_HEIGHT", "1400")),
		CacheTTL:        mustParseDuration(getEnv("CACHE_TTL", "5m")),
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func mustParseInt(v string) int {
	i, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return i
}

func mustParseDuration(v string) time.Duration {
	t, err := time.ParseDuration(v)
	if err != nil {
		return 5 * time.Minute
	}
	return t
}
