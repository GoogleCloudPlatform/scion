// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// DefaultPort is the default listening port for the MCP server listener.
	DefaultPort = 9820
	// DefaultPath is the default HTTP path for the streamable MCP endpoint.
	DefaultPath = "/mcp"
	// DefaultPrincipalCacheTTL is the maximum TTL for /api/v1/auth/me caching.
	DefaultPrincipalCacheTTL = 60 * time.Second
)

// Options configures a Scion MCP server.
type Options struct {
	Host            string
	Port            int
	Path            string
	PublicURL       string
	AllowedOrigins  []string
	Version         string
	Backend         HubBackend
	Logger          *slog.Logger
	PrincipalTTL    time.Duration
	ShutdownTimeout time.Duration
	// ExtraTools is an optional extension hook allowing callers (e.g. custom
	// wrappers) to register additional tools on the underlying *mcp.Server.
	ExtraTools func(*mcp.Server)
}

// Server serves the Scion MCP endpoint on its own HTTP listener alongside
// /healthz and /readyz.
type Server struct {
	opts           Options
	backend        HubBackend
	logger         *slog.Logger
	allowedOrigins map[string]struct{}
	principalCache *principalCache
	mcpServer      *mcp.Server
	handler        http.Handler

	shuttingDown atomic.Bool

	mu       sync.Mutex
	httpSrv  *http.Server
	listener net.Listener
}

// New creates a new MCP Server. It does not bind a listener until Start or
// Serve is called.
func New(opts Options) (*Server, error) {
	if opts.Backend == nil {
		return nil, errors.New("mcpserver: Backend is required")
	}
	if opts.Path == "" {
		opts.Path = DefaultPath
	}
	if !strings.HasPrefix(opts.Path, "/") {
		opts.Path = "/" + opts.Path
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	if opts.PrincipalTTL <= 0 || opts.PrincipalTTL > DefaultPrincipalCacheTTL {
		opts.PrincipalTTL = DefaultPrincipalCacheTTL
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = 10 * time.Second
	}

	origins := make(map[string]struct{}, len(opts.AllowedOrigins))
	for _, o := range opts.AllowedOrigins {
		if trimmed := strings.TrimSpace(o); trimmed != "" {
			origins[trimmed] = struct{}{}
		}
	}

	s := &Server{
		opts:           opts,
		backend:        opts.Backend,
		logger:         opts.Logger,
		allowedOrigins: origins,
		principalCache: newPrincipalCache(opts.PrincipalTTL),
	}

	mcpSrv := mcp.NewServer(&mcp.Implementation{
		Name:    "scion-mcp",
		Version: opts.Version,
	}, &mcp.ServerOptions{
		Logger: opts.Logger,
	})
	s.registerTools(mcpSrv)
	if opts.ExtraTools != nil {
		opts.ExtraTools(mcpSrv)
	}
	s.mcpServer = mcpSrv

	streamable := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s.mcpServer },
		&mcp.StreamableHTTPOptions{
			Stateless:                  true,
			Logger:                     opts.Logger,
			DisableLocalhostProtection: true,
		},
	)

	mux := http.NewServeMux()
	mux.Handle(opts.Path, s.frontDoor(streamable))
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	s.handler = mux

	return s, nil
}

// Handler returns the HTTP handler serving /mcp, /healthz, and /readyz.
func (s *Server) Handler() http.Handler {
	return s.handler
}

// BeginShutdown marks the MCP server as shutting down so /readyz and new /mcp
// requests immediately answer 503 Service Unavailable. Safe to call multiple
// times and suitable for shutdownSeq.addStep(shutdownPhaseMark, ...).
func (s *Server) BeginShutdown() {
	s.shuttingDown.Store(true)
}

// Start binds Host:Port and serves until ctx is cancelled or the listener fails.
// On context cancellation it calls BeginShutdown and drains active requests via
// http.Server.Shutdown within opts.ShutdownTimeout.
func (s *Server) Start(ctx context.Context) error {
	port := s.opts.Port
	if port == 0 {
		port = DefaultPort
	}
	addr := net.JoinHostPort(s.opts.Host, fmt.Sprintf("%d", port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("mcpserver: listen %s: %w", addr, err)
	}
	return s.Serve(ctx, ln)
}

// Serve serves on an already-bound net.Listener until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	httpSrv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	s.mu.Lock()
	s.listener = ln
	s.httpSrv = httpSrv
	s.mu.Unlock()

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		s.BeginShutdown()
		shutCtx, cancel := context.WithTimeout(context.Background(), s.opts.ShutdownTimeout)
		defer cancel()
		shutErr := httpSrv.Shutdown(shutCtx)
		serveErr := <-errCh
		if shutErr != nil {
			return shutErr
		}
		return serveErr
	}
}

// Shutdown marks the server shutting down and gracefully shuts down the HTTP
// server using ctx.
func (s *Server) Shutdown(ctx context.Context) error {
	s.BeginShutdown()
	s.mu.Lock()
	httpSrv := s.httpSrv
	s.mu.Unlock()
	if httpSrv == nil {
		return nil
	}
	return httpSrv.Shutdown(ctx)
}

// Addr returns the bound listener address, or "" if not yet listening.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if s.shuttingDown.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "shutting_down"})
		}
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}
