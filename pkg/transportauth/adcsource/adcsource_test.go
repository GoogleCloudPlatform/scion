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

package adcsource

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	"google.golang.org/api/idtoken"
)

func makeTestJWT(exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]interface{}{"exp": exp.Unix(), "iss": "test"})
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	sig := base64.RawURLEncoding.EncodeToString([]byte("fakesig"))
	return fmt.Sprintf("%s.%s.%s", header, payloadB64, sig)
}

// mockTokenSource implements oauth2.TokenSource for testing.
type mockTokenSource struct {
	idToken   string
	err       error
	callCount atomic.Int32
}

func (m *mockTokenSource) Token() (*oauth2.Token, error) {
	m.callCount.Add(1)
	if m.err != nil {
		return nil, m.err
	}
	tok := &oauth2.Token{
		AccessToken: "access-token",
		TokenType:   "Bearer",
	}
	// Set id_token as extra field, matching what idtoken.NewTokenSource returns.
	return tok.WithExtra(map[string]interface{}{"id_token": m.idToken}), nil
}

func overrideNewTokenSource(ts oauth2.TokenSource, err error) func() {
	orig := newTokenSourceFunc
	newTokenSourceFunc = func(_ context.Context, _ string, _ ...idtoken.ClientOption) (oauth2.TokenSource, error) {
		if err != nil {
			return nil, err
		}
		return ts, nil
	}
	return func() { newTokenSourceFunc = orig }
}

func TestADCSource_SetAndGet(t *testing.T) {
	src := &ADCSource{audience: "https://hub.example.com"}
	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	expiry := time.Now().Add(1 * time.Hour)

	src.SetToken(token, expiry)

	got, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, token, got)
}

func TestADCSource_FetchViaMock(t *testing.T) {
	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	mock := &mockTokenSource{idToken: token}
	cleanup := overrideNewTokenSource(mock, nil)
	defer cleanup()

	src := &ADCSource{audience: "https://hub.example.com"}
	got, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, token, got)
	assert.Equal(t, int32(1), mock.callCount.Load())

	// Second call should use cache.
	got2, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, token, got2)
	assert.Equal(t, int32(1), mock.callCount.Load(), "should use cached token")
}

func TestADCSource_FetchError(t *testing.T) {
	cleanup := overrideNewTokenSource(nil, fmt.Errorf("ADC not configured"))
	defer cleanup()

	src := &ADCSource{audience: "https://hub.example.com"}
	_, err := src.Token()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ADC token source")
}

func TestADCSource_TokenError(t *testing.T) {
	mock := &mockTokenSource{err: fmt.Errorf("token fetch failed")}
	cleanup := overrideNewTokenSource(mock, nil)
	defer cleanup()

	src := &ADCSource{audience: "https://hub.example.com"}
	_, err := src.Token()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ADC token fetch")
}

func TestADCSource_UpdateToken(t *testing.T) {
	src := &ADCSource{audience: "https://hub.example.com"}
	token1 := makeTestJWT(time.Now().Add(1 * time.Hour))
	token2 := makeTestJWT(time.Now().Add(2 * time.Hour))

	src.SetToken(token1, time.Now().Add(1*time.Hour))
	got1, _ := src.Token()
	assert.Equal(t, token1, got1)

	src.SetToken(token2, time.Now().Add(2*time.Hour))
	got2, _ := src.Token()
	assert.Equal(t, token2, got2)
}

func TestADCSource_Expiry(t *testing.T) {
	src := &ADCSource{audience: "https://hub.example.com"}
	assert.True(t, src.Expiry().IsZero())

	expiry := time.Now().Add(1 * time.Hour)
	src.SetToken("tok", expiry)
	assert.WithinDuration(t, expiry, src.Expiry(), time.Second)
}

func TestADCSource_Audience(t *testing.T) {
	src := &ADCSource{audience: "https://hub.example.com"}
	assert.Equal(t, "https://hub.example.com", src.Audience())
}

func TestADCSource_CachedTokenReturned(t *testing.T) {
	src := &ADCSource{audience: "https://hub.example.com"}
	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	src.SetToken(token, time.Now().Add(1*time.Hour))

	// Multiple calls should return the cached token without error.
	for i := 0; i < 5; i++ {
		got, err := src.Token()
		require.NoError(t, err)
		assert.Equal(t, token, got)
	}
}

func TestADCSource_RefreshWithinMargin(t *testing.T) {
	src := &ADCSource{audience: "https://hub.example.com"}
	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	src.SetToken(token, time.Now().Add(3*time.Minute)) // within 5-min refresh margin

	// Token is within the refresh margin, so Token() should try to refresh.
	// Without a working idtoken source, this will error.
	_, err := src.Token()
	assert.Error(t, err, "should attempt refresh when within margin")
}

func TestADCSource_ImplementsTokenSource(t *testing.T) {
	var _ transportauth.TokenSource = (*ADCSource)(nil)
}
