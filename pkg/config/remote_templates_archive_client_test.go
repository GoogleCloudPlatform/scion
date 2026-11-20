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

package config

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/ssrfclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// publicResolver resolves every host to a public address.
type publicResolver struct{}

func (publicResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
}

// fixedDialer dials addr whatever address it is asked for.
type fixedDialer struct{ addr string }

func (d fixedDialer) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	var nd net.Dialer
	return nd.DialContext(ctx, network, d.addr)
}

// An ArchiveClient replaces http.DefaultClient for archive downloads, so a
// caller's destination restrictions apply.
func TestFetchRemoteTemplateWithOptions_ArchiveClientBlocksLoopback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	_, err := FetchRemoteTemplateWithOptions(context.Background(), srv.URL+"/x.tar.gz",
		FetchOptions{ArchiveClient: ssrfclient.NewClient(ssrfclient.Config{})})
	require.Error(t, err)
	assert.ErrorIs(t, err, ssrfclient.ErrBlockedAddress)
	assert.Zero(t, hits)
}

func TestFetchRemoteTemplateWithOptions_ArchiveClientRefusesRedirect(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/x.tar.gz", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	client := ssrfclient.NewClient(ssrfclient.Config{
		Resolver: publicResolver{},
		Dialer:   fixedDialer{addr: srv.Listener.Addr().String()},
	})
	_, err := FetchRemoteTemplateWithOptions(context.Background(), "http://archive.example/x.tar.gz",
		FetchOptions{ArchiveClient: client})
	require.Error(t, err)
	assert.ErrorIs(t, err, ssrfclient.ErrRedirectBlocked)
}
