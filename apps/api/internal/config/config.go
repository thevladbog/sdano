// Package config loads all runtime configuration from the environment
// (12-factor: no config files, every variable listed in .env.example).
package config

import (
	"errors"
	"fmt"
	"strconv"
)

type Config struct {
	HTTPAddr       string
	DatabaseURL    string
	S3Endpoint     string
	S3Region       string
	S3Bucket       string
	S3AccessKey    string
	S3SecretKey    string
	S3UsePathStyle bool
	AdminOrigin    string // CORS origin; wired when apps/admin exists
	JWTSecret      string
}

func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		HTTPAddr:    withDefault(getenv("HTTP_ADDR"), ":8080"),
		DatabaseURL: getenv("DATABASE_URL"),
		S3Endpoint:  getenv("S3_ENDPOINT"),
		S3Region:    withDefault(getenv("S3_REGION"), "us-east-1"),
		S3Bucket:    getenv("S3_BUCKET"),
		S3AccessKey: getenv("S3_ACCESS_KEY"),
		S3SecretKey: getenv("S3_SECRET_KEY"),
		AdminOrigin: getenv("ADMIN_ORIGIN"),
		JWTSecret:   getenv("JWT_SECRET"),
	}

	var err error
	if cfg.S3UsePathStyle, err = parseBool(getenv, "S3_USE_PATH_STYLE"); err != nil {
		return Config{}, err
	}

	var missing []string
	for name, v := range map[string]string{
		"DATABASE_URL":  cfg.DatabaseURL,
		"S3_ENDPOINT":   cfg.S3Endpoint,
		"S3_BUCKET":     cfg.S3Bucket,
		"S3_ACCESS_KEY": cfg.S3AccessKey,
		"S3_SECRET_KEY": cfg.S3SecretKey,
		"JWT_SECRET":    cfg.JWTSecret,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required env vars: %v", missing)
	}

	const minJWTSecretLen = 32
	if len(cfg.JWTSecret) < minJWTSecretLen {
		return Config{}, fmt.Errorf("JWT_SECRET must be at least %d bytes for HS256 security", minJWTSecretLen)
	}
	return cfg, nil
}

func withDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func parseBool(getenv func(string) string, name string) (bool, error) {
	raw := getenv(name)
	if raw == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false, errors.Join(fmt.Errorf("parsing %s", name), err)
	}
	return b, nil
}
