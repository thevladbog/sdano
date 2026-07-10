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
		"JWT_SECRET":    "test-secret-at-least-32-bytes-long!!",
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
	if cfg.JWTSecret != "test-secret-at-least-32-bytes-long!!" {
		t.Errorf("JWTSecret = %q, want test-secret-at-least-32-bytes-long!!", cfg.JWTSecret)
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

func TestLoadRejectsShortJWTSecret(t *testing.T) {
	env := validEnv()
	env["JWT_SECRET"] = "short"
	if _, err := Load(fakeEnv(env)); err == nil {
		t.Fatal("Load must fail when JWT_SECRET is shorter than 32 bytes")
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

func TestLoadDefaultsTrustedProxyCountToZero(t *testing.T) {
	cfg, err := Load(fakeEnv(validEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TrustedProxyCount != 0 {
		t.Errorf("TrustedProxyCount default = %d, want 0", cfg.TrustedProxyCount)
	}
}

func TestLoadParsesTrustedProxyCount(t *testing.T) {
	env := validEnv()
	env["TRUSTED_PROXY_COUNT"] = "1"
	cfg, err := Load(fakeEnv(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TrustedProxyCount != 1 {
		t.Errorf("TrustedProxyCount = %d, want 1", cfg.TrustedProxyCount)
	}
}

func TestLoadRejectsNegativeTrustedProxyCount(t *testing.T) {
	env := validEnv()
	env["TRUSTED_PROXY_COUNT"] = "-1"
	if _, err := Load(fakeEnv(env)); err == nil {
		t.Fatal("Load must reject a negative TRUSTED_PROXY_COUNT")
	}
}

func TestLoadRejectsNonNumericTrustedProxyCount(t *testing.T) {
	env := validEnv()
	env["TRUSTED_PROXY_COUNT"] = "notanumber"
	if _, err := Load(fakeEnv(env)); err == nil {
		t.Fatal("Load must reject a non-numeric TRUSTED_PROXY_COUNT")
	}
}

func TestLoadDefaultsChromeCDPURL(t *testing.T) {
	cfg, err := Load(fakeEnv(validEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ChromeCDPURL != "http://localhost:9222" {
		t.Errorf("ChromeCDPURL default = %q, want http://localhost:9222", cfg.ChromeCDPURL)
	}
}

func TestLoadReadsChromeCDPURL(t *testing.T) {
	env := validEnv()
	env["CHROME_CDP_URL"] = "http://headless-shell:9222"
	cfg, err := Load(fakeEnv(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ChromeCDPURL != "http://headless-shell:9222" {
		t.Errorf("ChromeCDPURL = %q, want http://headless-shell:9222", cfg.ChromeCDPURL)
	}
}
