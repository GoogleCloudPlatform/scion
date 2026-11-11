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

package hub

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"cloud.google.com/go/storage"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
)

// gcsLinkReadOnlyScope is the only OAuth scope ever requested for a gs://
// link fetch. It is strictly narrower than the cloud-platform scope agents
// get from handleAgentGCPToken, so a token minted for this feature can only
// read Cloud Storage even if it leaked.
const gcsLinkReadOnlyScope = "https://www.googleapis.com/auth/devstorage.read_only"

// gcsObjectSource is the seam between the endpoint and the actual GCS
// client, so tests can substitute an in-memory fake with call counters.
// Open pins the read to the exact generation Attrs reported, so a
// concurrent overwrite between the two calls is a precondition failure,
// never different bytes.
type gcsObjectSource interface {
	Attrs(ctx context.Context, bucket, object string) (*storage.ObjectAttrs, error)
	Open(ctx context.Context, bucket, object string, generation int64) (io.ReadCloser, error)
}

// gcsSourceFactory returns a gcsObjectSource authenticated as saEmail, and a
// release func the caller must call exactly once (e.g. via defer) when done
// with it. The production factory (gcsObjectSourceFor) mints a fresh token
// and builds a new client for every call. It is consulted only after every
// authorization step (1-9) has already passed.
type gcsSourceFactory func(ctx context.Context, saEmail string) (gcsObjectSource, func(), error)

// gcsClientSource adapts a *storage.Client to gcsObjectSource.
type gcsClientSource struct {
	client *storage.Client
}

func (s *gcsClientSource) Attrs(ctx context.Context, bucket, object string) (*storage.ObjectAttrs, error) {
	return s.client.Bucket(bucket).Object(object).Attrs(ctx)
}

// Open reads the object, constrained to the exact generation the caller
// already observed via Attrs. This is implemented as an If(GenerationMatch)
// read precondition rather than an explicit Generation(gen) address: the
// latter addresses a specific (possibly no-longer-live) generation number,
// which GCS answers with a plain 404 once the bucket overwrites it unless
// object versioning happens to be on — indistinguishable from "never
// existed" and impossible to map to the required 502. A precondition on
// the live object, by contrast, always surfaces a distinguishable 412 on
// mismatch regardless of versioning, so a concurrent overwrite between
// Attrs and Open reliably maps to a 502, never a partial or substituted
// body.
func (s *gcsClientSource) Open(ctx context.Context, bucket, object string, generation int64) (io.ReadCloser, error) {
	obj := s.client.Bucket(bucket).Object(object).
		If(storage.Conditions{GenerationMatch: generation}).
		ReadCompressed(true)
	return obj.NewReader(ctx)
}

// errGCSLinkNoTokenGenerator is returned by gcsObjectSourceFor if the hub's
// token generator is cleared between step 1's check and this call (a narrow
// race, since nothing caches a generator reference across requests — step 1
// and this call are both within the same request).
var errGCSLinkNoTokenGenerator = errors.New("gcs link: no GCP token generator configured")

// newDefaultHTTPTransport reproduces the settings of Go's own
// http.DefaultTransport (net/http/transport.go), for use when
// http.DefaultTransport itself is not a *http.Transport (see
// newGCSLinkBaseTransport) and so cannot be cloned directly.
func newDefaultHTTPTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// newGCSLinkBaseTransport builds the one shared, credential-free base
// transport every per-request storage client's oauth2.Transport wraps.
// Cloned from http.DefaultTransport, not reused directly, so this
// package's own connection pool is never shared with (and so never
// mutated by) anything else in the process that might hold a
// reference to http.DefaultTransport. Shared across every request so
// independent requests still reuse pooled, keep-alive connections to GCS
// instead of each dialing fresh; it carries no credentials of its own —
// each request's oauth2.Transport supplies its own StaticTokenSource over
// this Base, so the transport itself cannot leak one request's token to
// another. No custom timeouts: the defaults (keep-alive, idle-connection
// behavior, etc.) are the same ones Go's own http.DefaultTransport uses for
// every other outbound call in this process, and request-level bounding
// comes from the context passed to each call, not from the transport.
//
// http.DefaultTransport is a package-level var of type http.RoundTripper,
// not *http.Transport, so a process that replaces or wraps it (e.g. an
// otel/tracing instrumentation shim) means a bare, unconditional type
// assertion to *http.Transport is not safe here. The comma-ok form below
// falls back to newDefaultHTTPTransport() instead — a fresh, credential-free
// transport with the same settings — rather than reusing the replaced
// RoundTripper directly, which could be an arbitrary wrapper this package
// has no control over (and, unlike a plain *http.Transport, cannot safely
// Clone()).
func newGCSLinkBaseTransport() *http.Transport {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		return t.Clone()
	}
	return newDefaultHTTPTransport()
}

// gcsObjectSourceFor is the production gcsSourceFactory. Only ever
// consulted after every authorization step (1-9) has already passed. Per
// request: mints a devstorage.read_only token for saEmail using
// ctx — the request's own context, already bounded by
// gcsLinkEffectiveRequestDeadline — then builds a *storage.Client whose HTTP
// client is an oauth2.Transport over s.gcsLinkBaseTransport, wrapping that
// one token via oauth2.StaticTokenSource. There is no per-SA cache and no
// token refresh: the client is used for exactly one request and closed by
// the returned release func when the caller is done with it, so a hung mint
// is bounded by the same ctx as the rest of the request, and two requests
// for different SAs can never share, or be confused about, a token.
func (s *Server) gcsObjectSourceFor(ctx context.Context, saEmail string) (gcsObjectSource, func(), error) {
	gen := s.gcpTokenGenerator
	if gen == nil {
		return nil, func() {}, errGCSLinkNoTokenGenerator
	}
	tok, err := gen.GenerateAccessToken(ctx, saEmail, []string{gcsLinkReadOnlyScope})
	if err != nil {
		return nil, func() {}, err
	}
	ts := oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken: tok.AccessToken,
		TokenType:   tok.TokenType,
		Expiry:      time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second),
	})
	httpClient := &http.Client{Transport: &oauth2.Transport{Source: ts, Base: s.gcsLinkBaseTransport}}
	// WithoutAuthentication prevents storage.NewClient from running its own
	// ADC detection (reading GOOGLE_APPLICATION_CREDENTIALS or the
	// well-known file, and probing metadata.OnGCE) and from attaching any
	// detected credential to the client. The client already carries the
	// minted bearer token via httpClient's oauth2.Transport, which takes
	// precedence for every actual request; nothing here ever reads or holds
	// the hub's own credential.
	clientOpts := []option.ClientOption{option.WithHTTPClient(httpClient), option.WithoutAuthentication()}
	if s.gcsLinkEndpointOverride != "" {
		clientOpts = append(clientOpts, option.WithEndpoint(s.gcsLinkEndpointOverride))
	}
	client, err := storage.NewClient(ctx, clientOpts...)
	if err != nil {
		return nil, func() {}, err
	}
	return &gcsClientSource{client: client}, func() { _ = client.Close() }, nil
}

// gcsLinkEffectiveRequestDeadline is the whole-request deadline actually
// used for s: s.gcsLinkRequestDeadlineOverride if a test has set one, else
// the production constant gcsLinkRequestDeadline. A test needs to pin a
// short deadline (e.g. to bound how long it waits for a stalled response
// write, or a hung mint) without changing the production default.
func gcsLinkEffectiveRequestDeadline(s *Server) time.Duration {
	if s.gcsLinkRequestDeadlineOverride > 0 {
		return s.gcsLinkRequestDeadlineOverride
	}
	return gcsLinkRequestDeadline
}
