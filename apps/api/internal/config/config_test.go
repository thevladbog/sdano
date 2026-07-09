package config

import "testing"

func fakeEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(fakeEnv(map[string]string{
		"DATABASE_URL":  "postgres://u:p@localhost:5432/db",
		"S3_ENDPOINT":   "http://localhost:9000",
		"S3_BUCKET":     "sdano-evidence",
		"S3_ACCESS_KEY": "k",
		"S3_SECRET_KEY": "s",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr default = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.S3Region != "us-east-1" {
		t.Errorf("S3Region default = %q, want us-east-1", cfg.S3Region)
	}
	if cfg.DevTenantHeaderAuth {
		t.Error("DevTenantHeaderAuth must default to false")
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	_, err := Load(fakeEnv(map[string]string{
		"S3_ENDPOINT": "e", "S3_BUCKET": "b", "S3_ACCESS_KEY": "k", "S3_SECRET_KEY": "s",
	}))
	if err == nil {
		t.Fatal("Load must fail without DATABASE_URL")
	}
}

func TestLoadParsesBools(t *testing.T) {
	cfg, err := Load(fakeEnv(map[string]string{
		"DATABASE_URL": "d", "S3_ENDPOINT": "e", "S3_BUCKET": "b",
		"S3_ACCESS_KEY": "k", "S3_SECRET_KEY": "s",
		"S3_USE_PATH_STYLE": "true", "DEV_TENANT_HEADER_AUTH": "true",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.S3UsePathStyle || !cfg.DevTenantHeaderAuth {
		t.Error("boolean env vars 'true' must parse to true")
	}
}
