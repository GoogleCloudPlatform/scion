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

// Package auth provides authentication utilities for CLI-to-Hub communication.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	// CallbackPort is the port for the localhost callback server.
	CallbackPort = 18271
	// CallbackPath is the path for the OAuth callback.
	CallbackPath = "/callback"
	// DefaultTimeout is the default timeout for waiting for authentication.
	DefaultTimeout = 5 * time.Minute
)

// authSuccessHTML is the HTML page shown after successful authentication.
const authSuccessHTML = `<!DOCTYPE html>
<html>
<head>
    <title>Authentication Successful</title>
    <style>
        body {
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
            display: flex;
            justify-content: center;
            align-items: center;
            height: 100vh;
            margin: 0;
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
        }
        .container {
            text-align: center;
            background: white;
            padding: 40px 60px;
            border-radius: 12px;
            box-shadow: 0 10px 40px rgba(0,0,0,0.2);
        }
        .checkmark {
            font-size: 64px;
            margin-bottom: 20px;
        }
        h1 {
            color: #333;
            margin: 0 0 10px 0;
        }
        p {
            color: #666;
            margin: 0;
        }
    </style>
</head>
<body>
    <div class="container">
        <div class="checkmark">&#10004;</div>
        <h1>Authentication Successful</h1>
        <p>You can close this window and return to the terminal.</p>
    </div>
</body>
</html>`

// authErrorHTML is the HTML template for authentication errors.
const authErrorHTML = `<!DOCTYPE html>
<html>
<head>
    <title>Authentication Failed</title>
    <style>
        body {
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
            display: flex;
            justify-content: center;
            align-items: center;
            height: 100vh;
            margin: 0;
            background: linear-gradient(135deg, #e74c3c 0%%, #c0392b 100%%);
        }
        .container {
            text-align: center;
            background: white;
            padding: 40px 60px;
            border-radius: 12px;
            box-shadow: 0 10px 40px rgba(0,0,0,0.2);
        }
        .error-icon {
            font-size: 64px;
            margin-bottom: 20px;
        }
        h1 {
            color: #333;
            margin: 0 0 10px 0;
        }
        p {
            color: #666;
            margin: 0;
        }
    </style>
</head>
<body>
    <div class="container">
        <div class="error-icon">&#10008;</div>
        <h1>Authentication Failed</h1>
        <p>%s</p>
    </div>
</body>
</html>`

// LocalhostAuthServer handles the OAuth callback on localhost.
type LocalhostAuthServer struct {
	server   *http.Server
	listener net.Listener
	codeChan chan string
	errChan  chan error
	state    string
	port     int
	mu       sync.Mutex
	started  bool
}

// NewLocalhostAuthServer creates a new localhost authentication server.
func NewLocalhostAuthServer() *LocalhostAuthServer {
	return &LocalhostAuthServer{
		codeChan: make(chan string, 1),
		errChan:  make(chan error, 1),
		port:     CallbackPort,
	}
}

// WithPort sets a custom port for the server.
func (s *LocalhostAuthServer) WithPort(port int) *LocalhostAuthServer {
	s.port = port
	return s
}

// Start starts the localhost callback server and returns the callback URL.
func (s *LocalhostAuthServer) Start(ctx context.Context) (callbackURL string, state string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return "", "", fmt.Errorf("server already started")
	}

	// Generate random state for CSRF protection
	s.state, err = generateRandomState()
	if err != nil {
		return "", "", fmt.Errorf("failed to generate state: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(CallbackPath, s.handleCallback)

	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	s.listener, err = net.Listen("tcp", addr)
	if err != nil {
		return "", "", fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	s.server = &http.Server{
		Handler: mux,
	}

	s.started = true

	// Start server in background
	go func() {
		if err := s.server.Serve(s.listener); err != nil && err != http.ErrServerClosed {
			select {
			case s.errChan <- err:
			default:
			}
		}
	}()

	callbackURL = fmt.Sprintf("http://127.0.0.1:%d%s", s.port, CallbackPath)
	return callbackURL, s.state, nil
}

// handleCallback handles the OAuth callback request.
func (s *LocalhostAuthServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	// Verify state matches (CSRF protection)
	state := r.URL.Query().Get("state")
	if state != s.state {
		s.sendError(fmt.Errorf("state mismatch"))
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, authErrorHTML, "State mismatch - possible CSRF attack")
		return
	}

	// Check for error response
	if errCode := r.URL.Query().Get("error"); errCode != "" {
		errDesc := r.URL.Query().Get("error_description")
		if errDesc == "" {
			errDesc = errCode
		}
		s.sendError(fmt.Errorf("auth failed: %s", errDesc))
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, authErrorHTML, errDesc)
		return
	}

	// Get authorization code
	code := r.URL.Query().Get("code")
	if code == "" {
		s.sendError(fmt.Errorf("no authorization code received"))
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, authErrorHTML, "No authorization code received")
		return
	}

	// Send success page to browser
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(authSuccessHTML))

	// Send code through channel
	select {
	case s.codeChan <- code:
	default:
		// Channel full, ignore duplicate callback
	}
}

// sendError sends an error through the error channel.
func (s *LocalhostAuthServer) sendError(err error) {
	select {
	case s.errChan <- err:
	default:
		// Channel full, ignore duplicate error
	}
}

// WaitForCode waits for the authorization code or timeout.
func (s *LocalhostAuthServer) WaitForCode(ctx context.Context) (string, error) {
	select {
	case code := <-s.codeChan:
		return code, nil
	case err := <-s.errChan:
		return "", err
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(DefaultTimeout):
		return "", fmt.Errorf("authentication timeout after %v", DefaultTimeout)
	}
}

// Shutdown gracefully shuts down the server.
func (s *LocalhostAuthServer) Shutdown() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started || s.server == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s.started = false
	return s.server.Shutdown(ctx)
}

// GetState returns the state parameter for this session.
func (s *LocalhostAuthServer) GetState() string {
	return s.state
}

// generateRandomState generates a cryptographically random state string.
func generateRandomState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}
