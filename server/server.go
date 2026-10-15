// Package server assembles the HTTP handler: both Connect services, the
// health check and the embedded web UI, for a host program to mount.
//
// The host owns people, teams and authentication. It supplies a Directory of
// people and teams and authenticates every request before it reaches the
// handler, which performs no authentication at all. For an AgentService call
// the host binds the caller's person with WithPerson; a call without one is
// refused as Unauthenticated. An AdminService call is served to whoever the
// host lets through.
package server

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1/harnessctlv1connect"
	"github.com/tetriz-ai/tetriz-harness/server/blob"
	"github.com/tetriz-ai/tetriz-harness/server/service"
)

// Request size caps: an admin upsert may carry up to 100 files of 1 MiB each
// in principle, but a whole request is bounded well below that.
const (
	adminReadMaxBytes = 32 << 20
	agentReadMaxBytes = 8 << 20
)

// Path prefixes of the two services, for a host routing its authentication.
const (
	AdminPathPrefix = "/" + harnessctlv1connect.AdminServiceName + "/"
	AgentPathPrefix = "/" + harnessctlv1connect.AgentServiceName + "/"
)

// Directory is the host's record of people and teams, read and never
// written by the server. Ids are opaque non-empty strings of at most MaxIDLen
// bytes, stored in targets and reports as given.
type Directory = service.Directory

// Person is one entry of a Directory.
type Person = service.Person

// Team is one entry of a Directory; its members come from TeamMembers.
type Team = service.Team

// MaxIDLen bounds the person and team ids a Directory hands out.
const MaxIDLen = service.MaxIDLen

// WithPerson binds the person the host authenticated to an AgentService
// request's context. The id should be one the Directory lists: status only
// counts persons it lists.
func WithPerson(ctx context.Context, personID string) context.Context {
	return service.WithPerson(ctx, personID)
}

// Store is what the server needs from persistence; *store.Store implements it.
type Store interface {
	service.Store
	Ping(ctx context.Context) error
}

// Options configures New.
type Options struct {
	// Store is required.
	Store Store
	// Directory is required.
	Directory Directory
	// Blobs holds artifact bodies; required.
	Blobs      blob.Store
	AllowHooks bool
	Logger     *slog.Logger
	// UI is the built web UI; nil serves none.
	UI fs.FS
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Version is the build reported to admins.
	Version string
}

// logErrors logs every failed RPC and strips the detail of an Internal error
// so storage errors never reach a client.
func logErrors(log *slog.Logger) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			resp, err := next(ctx, req)
			if err == nil {
				return resp, nil
			}
			code := connect.CodeOf(err)
			if code == connect.CodeInternal || code == connect.CodeUnknown {
				log.ErrorContext(ctx, "rpc failed", "procedure", req.Spec().Procedure, "code", code.String(), "error", err)
				return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
			}
			log.InfoContext(ctx, "rpc rejected", "procedure", req.Spec().Procedure, "code", code.String(), "error", err)
			return nil, err
		}
	})
}

// New returns the server's root handler. It performs no authentication: the
// host must authenticate every request before it reaches the handler.
func New(o Options) (http.Handler, error) {
	switch {
	case o.Store == nil:
		return nil, errors.New("server: Options.Store is required")
	case o.Directory == nil:
		return nil, errors.New("server: Options.Directory is required")
	case o.Blobs == nil:
		return nil, errors.New("server: Options.Blobs is required")
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	cfg := service.Config{AllowHooks: o.AllowHooks, Now: o.Now, Version: o.Version}
	mux := http.NewServeMux()

	adminPath, adminHandler := harnessctlv1connect.NewAdminServiceHandler(
		service.NewAdmin(o.Store, o.Directory, o.Blobs, cfg),
		connect.WithInterceptors(logErrors(log)),
		connect.WithReadMaxBytes(adminReadMaxBytes),
	)
	mux.Handle(adminPath, adminHandler)

	agentPath, agentHandler := harnessctlv1connect.NewAgentServiceHandler(
		service.NewAgent(o.Store, o.Directory, o.Blobs, cfg, log),
		connect.WithInterceptors(logErrors(log)),
		connect.WithReadMaxBytes(agentReadMaxBytes),
	)
	mux.Handle(agentPath, agentHandler)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := o.Store.Ping(ctx); err != nil {
			log.WarnContext(ctx, "health check failed", "error", err)
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	if o.UI != nil {
		// Method-less: "GET /" conflicts with the method-less Connect prefixes.
		mux.Handle("/", spa(o.UI, adminPath, agentPath, apiPrefix))
	}
	return mux, nil
}

// apiPrefix is the path every Connect procedure of this API starts with, so a
// mistyped procedure 404s instead of returning the UI.
const apiPrefix = "/harnessctl."

// spa serves the built UI: an existing file as itself, and any other
// extension-less path as index.html so client-side routes survive a reload.
// API paths and missing files with an extension 404.
func spa(ui fs.FS, apiPrefixes ...string) http.Handler {
	files := http.FileServerFS(ui)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		for _, pre := range apiPrefixes {
			if strings.HasPrefix(r.URL.Path, pre) {
				http.NotFound(w, r)
				return
			}
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			files.ServeHTTP(w, r)
			return
		}
		if st, err := fs.Stat(ui, name); err == nil && !st.IsDir() {
			files.ServeHTTP(w, r)
			return
		}
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, ui, "index.html")
	})
}
