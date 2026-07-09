package config

import "testing"

func fakeEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func validEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":  "postgres://u:p@localhost:5432/db",
		"S3_ENDPOINT":   "http://localhost:9000",
		"S3_BUCKET":     "sdano-evidence",
		"S3_ACCESS_KEY": "k",
		"S3_SECRET_KEY": "s",
		"JWT_SECRET":    "secret",
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(fakeEnv(validEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr default = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.S3Region != "us-east-1" {
		t.Errorf("S3Region default = %q, want us-east-1", cfg.S3Region)
	}
	if cfg.JWTSecret != "secret" {
		t.Errorf("JWTSecret = %q, want secret", cfg.JWTSecret)
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	env := validEnv()
	delete(env, "DATABASE_URL")
	if _, err := Load(fakeEnv(env)); err == nil {
		t.Fatal("Load must fail without DATABASE_URL")
	}
}

func TestLoadRequiresJWTSecret(t *testing.T) {
	env := validEnv()
	delete(env, "JWT_SECRET")
	if _, err := Load(fakeEnv(env)); err == nil {
		t.Fatal("Load must fail without JWT_SECRET")
	}
}

func TestLoadParsesPathStyleBool(t *testing.T) {
	env := validEnv()
	env["S3_USE_PATH_STYLE"] = "true"
	cfg, err := Load(fakeEnv(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.S3UsePathStyle {
		t.Error("S3_USE_PATH_STYLE=true must parse to true")
	}
}
