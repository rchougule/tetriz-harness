package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/tetriz-ai/tetriz-harness/server/blob"
	"github.com/tetriz-ai/tetriz-harness/server/service"
)

type stubStore struct{ service.Store }

func (stubStore) Ping(context.Context) error           { return nil }
func (stubStore) Paused(context.Context) (bool, error) { return true, nil }

type stubDir struct{ Directory }

func TestNewRequiresDependencies(t *testing.T) {
	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		o    Options
		want string
	}{
		{"no store", Options{Directory: stubDir{}, Blobs: blobs}, "Store"},
		{"no directory", Options{Store: stubStore{}, Blobs: blobs}, "Directory"},
		{"no blobs", Options{Store: stubStore{}, Directory: stubDir{}}, "Blobs"},
	} {
		if _, err := New(tc.o); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to name %s", tc.name, err, tc.want)
		}
	}
}

// TestHandlerPerformsNoAuthentication leaves every credential to the host:
// no header is inspected, and an agent call needs only a bound person.
func TestHandlerPerformsNoAuthentication(t *testing.T) {
	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Options{Store: stubStore{}, Directory: stubDir{}, Blobs: blobs})
	if err != nil {
		t.Fatal(err)
	}
	host := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.Header.Get("X-Test-Person"); p != "" {
			r = r.WithContext(WithPerson(r.Context(), p))
		}
		h.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(host)
	defer srv.Close()

	tests := []struct {
		name       string
		procedure  string
		person     string
		bearer     string
		wantStatus int
	}{
		{"admin without any header", AdminPathPrefix + "GetSettings", "", "", http.StatusOK},
		{"admin with an arbitrary bearer", AdminPathPrefix + "GetSettings", "", "anything", http.StatusOK},
		{"agent without a person", AgentPathPrefix + "GetEnforcedSet", "", "anything", http.StatusUnauthorized},
		{"agent with a bound person", AgentPathPrefix + "GetEnforcedSet", "dev-1", "", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, srv.URL+tt.procedure, bytes.NewReader([]byte("{}")))
			req.Header.Set("Content-Type", "application/json")
			if tt.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tt.bearer)
			}
			if tt.person != "" {
				req.Header.Set("X-Test-Person", tt.person)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
		})
	}

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %v, %v", resp, err)
	}
	_ = resp.Body.Close()
}

func TestSPAFallback(t *testing.T) {
	ui := fstest.MapFS{
		"index.html":         {Data: []byte("<html>app</html>")},
		"assets/app-1a2b.js": {Data: []byte("console.log(1)")},
		"favicon.svg":        {Data: []byte("<svg/>")},
	}
	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Options{Store: stubStore{}, Directory: stubDir{}, Blobs: blobs, UI: ui})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantBody   string
	}{
		{"root", "/", http.StatusOK, "app"},
		{"client route", "/artifacts", http.StatusOK, "app"},
		{"nested client route", "/artifacts/00000000-0000-0000-0000-000000000001/edit", http.StatusOK, "app"},
		{"asset directory", "/assets", http.StatusOK, "app"},
		{"existing asset", "/assets/app-1a2b.js", http.StatusOK, "console.log"},
		{"existing root file", "/favicon.svg", http.StatusOK, "<svg/>"},
		{"missing asset", "/assets/app-old.js", http.StatusNotFound, ""},
		{"missing root file", "/robots.txt", http.StatusNotFound, ""},
		{"admin service path", "/harnessctl.v1.AdminService/ListArtifacts", http.StatusMethodNotAllowed, ""},
		{"unknown procedure", "/harnessctl.v1.NoSuchService/Call", http.StatusNotFound, ""},
		{"health", "/healthz", http.StatusOK, "ok"},
	}
	resp, err := http.Post(srv.URL+"/artifacts", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST to a client route = %d, want 405", resp.StatusCode)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Get(srv.URL + tt.path)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", resp.StatusCode, tt.wantStatus, body)
			}
			if !strings.Contains(string(body), tt.wantBody) {
				t.Fatalf("body = %q, want it to contain %q", body, tt.wantBody)
			}
			if tt.wantStatus == http.StatusNotFound && strings.Contains(string(body), "app") {
				t.Fatalf("404 served the UI: %q", body)
			}
		})
	}
}
