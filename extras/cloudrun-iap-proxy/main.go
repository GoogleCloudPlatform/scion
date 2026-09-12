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

package main

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// newProxy creates a ReverseProxy that forwards traffic to targetURL,
// preserving and logging Google Identity-Aware Proxy (IAP) headers.
func newProxy(targetURL *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = targetURL.Scheme
			req.URL.Host = targetURL.Host
			req.Host = targetURL.Host

			// Ensure all IAP headers are explicitly preserved and logged
			iapJWT := req.Header.Get("X-Goog-IAP-JWT-Assertion")
			iapEmail := req.Header.Get("X-Goog-Authenticated-User-Email")
			iapID := req.Header.Get("X-Goog-Authenticated-User-Id")

			if iapJWT != "" || iapEmail != "" {
				log.Printf("[Proxy] %s %s | IAP Auth Header Present | Email: %q | JWT Len: %d", req.Method, req.URL.Path, iapEmail, len(iapJWT))
			} else {
				log.Printf("[Proxy] %s %s | No IAP Auth Header Present", req.Method, req.URL.Path)
			}

			if iapJWT != "" {
				req.Header.Set("X-Goog-IAP-JWT-Assertion", iapJWT)
			}
			if iapEmail != "" {
				req.Header.Set("X-Goog-Authenticated-User-Email", iapEmail)
			}
			if iapID != "" {
				req.Header.Set("X-Goog-Authenticated-User-Id", iapID)
			}
		},
		FlushInterval: 100 * time.Millisecond,
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			log.Printf("[Proxy Error] %s %s: %v", req.Method, req.URL.Path, err)
			http.Error(w, "Bad Gateway (Proxy Failure)", http.StatusBadGateway)
		},
	}
}

// newMux returns an http.ServeMux configured with health check and reverse proxy endpoints.
func newMux(targetURL *url.URL) *http.ServeMux {
	proxy := newProxy(targetURL)
	mux := http.NewServeMux()

	// Cloud Run health check probe
	mux.HandleFunc("/proxy-healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok","service":"cloudrun-iap-proxy"}`))
	})

	// Reverse proxy for all other routes
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/proxy-healthz") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		proxy.ServeHTTP(w, r)
	})

	return mux
}

// parseTargetURL validates and parses the TARGET_URL string.
func parseTargetURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, fmt.Errorf("TARGET_URL is required (e.g. http://10.128.0.10:8080)")
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid TARGET_URL %q: %w", raw, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("TARGET_URL %q must specify a host", raw)
	}
	return u, nil
}

func main() {
	targetURLStr := os.Getenv("TARGET_URL")
	targetURL, err := parseTargetURL(targetURLStr)
	if err != nil {
		log.Fatalf("Configuration error: %v", err)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Starting Cloud Run IAP Reverse Proxy...")
	log.Printf("  Listening on port: %s", port)
	log.Printf("  Proxying to:       %s", targetURL.String())

	mux := newMux(targetURL)

	// Wrap handler with h2c for HTTP/2 cleartext support on Cloud Run
	h2s := &http2.Server{}
	h2cHandler := h2c.NewHandler(mux, h2s)

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      h2cHandler,
		ReadTimeout:  30 * time.Minute,
		WriteTimeout: 30 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	log.Printf("Proxy server active with H2C support on :%s", port)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server stopped: %v", err)
	}
}
