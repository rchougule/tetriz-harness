package config

import (
	"strings"
	"testing"
)

func TestFromEnv(t *testing.T) {
	base := map[string]string{"HARNESS_DATABASE_URL": "postgres://x", "HARNESS_DIRECTORY_FILE": "/etc/harness/directory.json"}
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
		check   func(Config) bool
	}{
		{"defaults", base, "", func(c Config) bool {
			return c.ListenAddr == ":8080" && c.BlobBackend == "fs" && !c.AllowHooks && c.S3UseSSL
		}},
		{"directory file required", map[string]string{"HARNESS_DATABASE_URL": "postgres://x"}, "HARNESS_DIRECTORY_FILE is required", nil},
		{"database required", map[string]string{"HARNESS_DIRECTORY_FILE": "d.json"}, "HARNESS_DATABASE_URL is required", nil},
		{"s3 needs bucket", merge(base, map[string]string{"HARNESS_BLOB_BACKEND": "s3"}), "HARNESS_S3_BUCKET", nil},
		{"unknown backend", merge(base, map[string]string{"HARNESS_BLOB_BACKEND": "gcs"}), "fs or s3", nil},
		{"bad bool", merge(base, map[string]string{"HARNESS_ALLOW_HOOKS": "maybe"}), "HARNESS_ALLOW_HOOKS", nil},
		{"hooks on", merge(base, map[string]string{"HARNESS_ALLOW_HOOKS": "true"}), "", func(c Config) bool { return c.AllowHooks }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := FromEnv(func(k string) string { return tt.env[k] })
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tt.check(c) {
				t.Fatalf("unexpected config %+v", c)
			}
		})
	}
}

func merge(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
