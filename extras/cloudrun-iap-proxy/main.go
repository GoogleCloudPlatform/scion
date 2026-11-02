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
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// forwardedProtoFor is the scheme the proxy reports for req: the scheme it
// served on this connection, https only when it terminated TLS itself and
// http otherwise. It is the single place that decides the reported scheme.
// On Cloud Run the platform front end terminates TLS and the proxy serves
// plain http, so it reports http: it cannot verify the front end's TLS, and
// a Scion hub that lists this proxy in server.trusted_proxies therefore
// refuses broker registration and join-token minting, broker join, secret
// rotation and broker connections through it (agent and web traffic are not
// affected; brokers reach the hub directly).
func forwardedProtoFor(req *http.Request) string {
	if req.TLS != nil {
		return "https"
	}
	return "http"
}

// setForwardingHeaders makes the proxy a clean trust boundary on out, the
// outbound request, from in, the request the proxy received. Every forwarding
// header the client sent is removed and the proxy states only what it
// observed itself; the Scion hub trusts X-Forwarded-Proto from a proxy listed
// in server.trusted_proxies, so a client value passed through would let any
// client claim https.
//   - Forwarded, X-Forwarded-Proto, X-Forwarded-For and X-Forwarded-Host are
//     removed from out, whatever their number or form.
//   - X-Forwarded-Proto is set to forwardedProtoFor(in).
//   - X-Forwarded-Host is set to in.Host, the host this request arrived with.
//   - X-Forwarded-For is set to the immediate peer's address alone.
//
// It runs in ReverseProxy.Rewrite, which runs after the hop-by-hop headers
// (including those a client names in Connection) are removed from out, so a
// client cannot strip the values set here.
func setForwardingHeaders(out, in *http.Request) {
	for _, h := range []string{"Forwarded", "X-Forwarded-Proto", "X-Forwarded-For", "X-Forwarded-Host"} {
		out.Header.Del(h)
	}
	out.Header.Set("X-Forwarded-Proto", forwardedProtoFor(in))
	if in.Host != "" {
		out.Header.Set("X-Forwarded-Host", in.Host)
	}
	if peer, _, err := net.SplitHostPort(in.RemoteAddr); err == nil && peer != "" {
		out.Header.Set("X-Forwarded-For", peer)
	}
}

// newProxy creates a ReverseProxy that forwards traffic to targetURL,
// preserving and logging Google Identity-Aware Proxy (IAP) headers and
// replacing every client forwarding header (setForwardingHeaders).
func newProxy(targetURL *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			out := pr.Out
			out.URL.Scheme = targetURL.Scheme
			out.URL.Host = targetURL.Host
			out.Host = targetURL.Host
			setForwardingHeaders(out, pr.In)

			// IAP headers pass through unchanged (Rewrite keeps every
			// non-hop-by-hop header); log whether they are present.
			iapJWT := out.Header.Get("X-Goog-IAP-JWT-Assertion")
			iapEmail := out.Header.Get("X-Goog-Authenticated-User-Email")
			if iapJWT != "" || iapEmail != "" {
				log.Printf("[Proxy] %s %s | IAP Auth Header Present | Email: %q | JWT Len: %d", out.Method, out.URL.Path, iapEmail, len(iapJWT))
			} else {
				log.Printf("[Proxy] %s %s | No IAP Auth Header Present", out.Method, out.URL.Path)
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
	log.Printf("  Client Forwarded and X-Forwarded-* headers are replaced with what this proxy observed")

	mux := newMux(targetURL)

	// Wrap handler with h2c for HTTP/2 cleartext support on Cloud Run
	h2s := &http2.Server{}
	h2cHandler := h2c.NewHandler(mux, h2s)

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           h2cHandler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Minute,
		WriteTimeout:      30 * time.Minute,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("Proxy server active with H2C support on :%s", port)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server stopped: %v", err)
	}
}
