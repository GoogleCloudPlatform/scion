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
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"k8s.io/client-go/kubernetes"

	"github.com/GoogleCloudPlatform/scion/third_party/ateapipb"
)

// Dial opens a gRPC connection to the ateapi Control service at
// cfg.APIEndpoint, authenticated with a TokenRequest-minted token for the
// calling pod's own ServiceAccount (refreshed automatically before each
// call), and with TLS verified against the CA configured in cfg.
// InsecureSkipVerify is never used — a cfg with no CA source is a
// configuration error (see serverTLSConfig).
func Dial(ctx context.Context, k8sClient kubernetes.Interface, cfg DialerConfig) (*grpc.ClientConn, error) {
	if cfg.APIEndpoint == "" {
		return nil, fmt.Errorf("substrate: api_endpoint is required")
	}

	tlsCfg, err := serverTLSConfig(ctx, k8sClient, cfg)
	if err != nil {
		return nil, err
	}

	ts, err := newTokenSource(k8sClient, cfg)
	if err != nil {
		return nil, err
	}

	conn, err := grpc.NewClient(cfg.APIEndpoint,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithPerRPCCredentials(ts),
	)
	if err != nil {
		return nil, fmt.Errorf("substrate: dial ateapi at %q: %w", cfg.APIEndpoint, err)
	}
	return conn, nil
}

// NewControlClient is a thin convenience wrapper so callers do not need to
// import ateapipb directly just to wrap the connection Dial returns.
func NewControlClient(conn *grpc.ClientConn) ateapipb.ControlClient {
	return ateapipb.NewControlClient(conn)
}
