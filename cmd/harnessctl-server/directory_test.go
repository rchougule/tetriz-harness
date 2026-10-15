package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tetriz-ai/tetriz-harness/server"
	"github.com/tetriz-ai/tetriz-harness/server/service"
)

func personOf(ctx context.Context) string {
	p, _ := service.PersonFrom(ctx)
	return p
}

func TestSampleDirectoryLoads(t *testing.T) {
	d, err := loadDirectory("directory.local.json")
	if err != nil {
		t.Fatal(err)
	}
	teams, _ := d.PersonTeams(context.Background(), "dev-1")
	if len(d.persons) != 1 || d.persons[0].ID != "dev-1" || len(teams) != 1 || teams[0] != "platform" {
		t.Fatalf("directory = %+v", d)
	}
}

func TestBuildDirectoryRejects(t *testing.T) {
	digest := sha256.Sum256([]byte("admin"))
	tests := []struct {
		name string
		json string
		want string
	}{
		{"no admin token", `{}`, "admin_token is empty"},
		{"bad digest", `{"admin_token":"sha256:abc"}`, "64 lowercase hex"},
		{"repeated person", `{"admin_token":"a","persons":[{"id":"p"},{"id":"p"}]}`, "repeated"},
		{"device token is the admin token", `{"admin_token":"sha256:` + hex.EncodeToString(digest[:]) + `","persons":[{"id":"p","device_tokens":["admin"]}]}`, "equals the admin token"},
		{"shared device token", `{"admin_token":"a","persons":[{"id":"p","device_tokens":["t"]},{"id":"q","device_tokens":["t"]}]}`, "another device"},
		{"unknown member", `{"admin_token":"a","teams":[{"id":"t","members":["ghost"]}]}`, "not a listed person"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := parseDirectory(strings.NewReader(tt.json))
			if err == nil {
				_, err = buildDirectory(f)
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestAuthenticate(t *testing.T) {
	devDigest := sha256.Sum256([]byte("dev-secret"))
	f, err := parseDirectory(strings.NewReader(`{"admin_token":"admin-secret","persons":[
		{"id":"dev-1","device_tokens":["sha256:` + hex.EncodeToString(devDigest[:]) + `"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	d, err := buildDirectory(f)
	if err != nil {
		t.Fatal(err)
	}
	var gotPerson string
	h := d.authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPerson = personOf(r.Context())
	}))
	tests := []struct {
		name, path, token string
		wantStatus        int
		wantPerson        string
	}{
		{"admin with admin token", server.AdminPathPrefix + "GetSettings", "admin-secret", 200, ""},
		{"admin without token", server.AdminPathPrefix + "GetSettings", "", 401, ""},
		{"admin with device token", server.AdminPathPrefix + "GetSettings", "dev-secret", 401, ""},
		{"agent with device token", server.AgentPathPrefix + "GetEnforcedSet", "dev-secret", 200, "dev-1"},
		{"agent with admin token", server.AgentPathPrefix + "GetEnforcedSet", "admin-secret", 401, ""},
		{"agent with unknown token", server.AgentPathPrefix + "GetEnforcedSet", "stranger", 401, ""},
		{"agent without token", server.AgentPathPrefix + "GetEnforcedSet", "", 401, ""},
		{"admin with unknown token", server.AdminPathPrefix + "GetSettings", "stranger", 401, ""},
		{"health passes through", "/healthz", "", 200, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPerson = ""
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus || gotPerson != tt.wantPerson {
				t.Fatalf("status %d person %q, want %d %q", rec.Code, gotPerson, tt.wantStatus, tt.wantPerson)
			}
			if tt.wantStatus == 401 && !strings.Contains(rec.Body.String(), `"code":"unauthenticated"`) {
				t.Fatalf("refusal body = %s, want a Connect unauthenticated error", rec.Body.String())
			}
		})
	}
}
