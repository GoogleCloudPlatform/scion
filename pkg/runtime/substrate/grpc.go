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

package substrate

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"k8s.io/client-go/kubernetes"

	ateapipb "github.com/GoogleCloudPlatform/scion/third_party/ateapipb"
)

// roundRobinServiceConfig spreads RPCs across every ateapi pod: the ateapi
// Service is headless (clusterIP: None), so the dns resolver returns one
// address per pod and the default pick_first policy would pin one.
const roundRobinServiceConfig = `{"loadBalancingConfig":[{"round_robin":{}}]}`

// Dial opens a gRPC connection to the ateapi Control service at
// cfg.APIEndpoint (as "dns:///<endpoint>", round_robin), authenticated
// with a TokenRequest-minted token for the calling pod's own ServiceAccount
// (refreshed automatically before each call) and, when cfg.ClientCertFile
// is set, a client certificate. TLS 1.3 is verified against the CA sources
// described on NewServerTLSSource, consulted on every handshake so a
// rotated CA applies to new connections. InsecureSkipVerify is never used:
// a cfg with no usable CA source is a configuration error.
func Dial(ctx context.Context, k8sClient kubernetes.Interface, cfg DialerConfig) (*grpc.ClientConn, error) {
	if cfg.APIEndpoint == "" {
		return nil, fmt.Errorf("substrate: api_endpoint is required")
	}

	src, err := NewServerTLSSource(ctx, k8sClient, cfg)
	if err != nil {
		return nil, err
	}

	ts, err := newTokenSource(k8sClient, cfg)
	if err != nil {
		return nil, err
	}

	target := dialTarget(cfg.APIEndpoint)
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(newSourceCredentials(src)),
		grpc.WithPerRPCCredentials(ts),
		grpc.WithDefaultServiceConfig(roundRobinServiceConfig),
	)
	if err != nil {
		return nil, fmt.Errorf("substrate: dial ateapi at %q: %w", target, err)
	}
	return conn, nil
}

// dialTarget returns the gRPC target for endpoint: unchanged when it already
// names a resolver scheme ("dns:///...", "passthrough:///..."), otherwise
// "dns:///<endpoint>".
func dialTarget(endpoint string) string {
	e := strings.TrimSpace(endpoint)
	if strings.Contains(e, "://") {
		return e
	}
	return "dns:///" + e
}

// sourceCredentials is a client-only credentials.TransportCredentials that
// obtains a fresh *tls.Config from a TLSConfigFunc for every handshake. It
// performs the handshake itself rather than delegating to
// credentials.NewTLS, because grpc's TLS credentials replace ServerName with
// the dial authority, which would discard an explicit server_name.
type sourceCredentials struct {
	src                TLSConfigFunc
	serverNameOverride string
}

func newSourceCredentials(src TLSConfigFunc) credentials.TransportCredentials {
	return &sourceCredentials{src: src}
}

// ClientHandshake implements credentials.TransportCredentials.
func (c *sourceCredentials) ClientHandshake(ctx context.Context, authority string, rawConn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	cfg, err := c.src(ctx)
	if err != nil {
		return nil, nil, err
	}
	cfg = cfg.Clone()
	switch {
	case c.serverNameOverride != "":
		cfg.ServerName = c.serverNameOverride
	case cfg.ServerName == "":
		cfg.ServerName = hostOnly(authority)
	}
	if !slices.Contains(cfg.NextProtos, http2ALPN) {
		cfg.NextProtos = append(slices.Clone(cfg.NextProtos), http2ALPN)
	}
	conn := tls.Client(rawConn, cfg)
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	state := conn.ConnectionState()
	if state.NegotiatedProtocol != http2ALPN {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("substrate: ateapi at %q did not negotiate ALPN %q", authority, http2ALPN)
	}
	return conn, credentials.TLSInfo{
		State:          state,
		CommonAuthInfo: credentials.CommonAuthInfo{SecurityLevel: credentials.PrivacyAndIntegrity},
	}, nil
}

// http2ALPN is the ALPN protocol gRPC requires over TLS.
const http2ALPN = "h2"

// ServerHandshake implements credentials.TransportCredentials. These
// credentials are client-only.
func (c *sourceCredentials) ServerHandshake(net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("substrate: ateapi transport credentials are client-only")
}

// Info implements credentials.TransportCredentials.
func (c *sourceCredentials) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "tls"}
}

// Clone implements credentials.TransportCredentials.
func (c *sourceCredentials) Clone() credentials.TransportCredentials {
	cp := *c
	return &cp
}

// OverrideServerName implements credentials.TransportCredentials.
func (c *sourceCredentials) OverrideServerName(name string) error {
	c.serverNameOverride = name
	return nil
}

// NewControlClient is a thin convenience wrapper so callers do not need to
// import ateapipb directly just to wrap the connection Dial returns.
func NewControlClient(conn *grpc.ClientConn) ateapipb.ControlClient {
	return ateapipb.NewControlClient(conn)
}
