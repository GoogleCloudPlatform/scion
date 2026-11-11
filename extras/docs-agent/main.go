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
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

//go:embed chat/*
var chatFS embed.FS

const (
	defaultPort      = "8080"
	defaultTimeout   = 60 * time.Second
	maxQueryLength   = 1000
	defaultWorkspace = "/workspace"
	defaultRepoDir   = "scion"
	defaultSystemMD  = "extras/docs-agent/system-prompt.md"
	defaultModel     = "gemini-3.1-flash-lite-preview"
)

var ansiRegexp = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// debugEnabled returns true when DEBUG=1 or DEBUG=true is set.
func debugEnabled() bool {
	v := os.Getenv("DEBUG")
	return v == "1" || strings.EqualFold(v, "true")
}

func debugf(format string, args ...any) {
	if debugEnabled() {
		log.Printf("[DEBUG] "+format, args...)
	}
}

// geminiNoiseRegexp matches gemini CLI diagnostic lines that leak into stdout.
var geminiNoiseRegexp = regexp.MustCompile(`(?m)^MCP issues detected\..*$\n?`)

// cleanGeminiOutput strips ANSI escape codes and gemini CLI noise from output.
func cleanGeminiOutput(s string) string {
	s = ansiRegexp.ReplaceAllString(s, "")
	s = geminiNoiseRegexp.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// shelljoin formats args as a copy-pasteable shell command fragment.
// Args containing spaces or special characters are single-quoted.
func shelljoin(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " \t\n\"'\\$`!#&|;(){}[]<>?*~") {
			parts[i] = "'" + strings.ReplaceAll(a, "'", "'\\''") + "'"
		} else {
			parts[i] = a
		}
	}
	return strings.Join(parts, " ")
}

// truncateTail returns the last maxLen bytes of s, prefixed with
// "...[truncated] " if truncation occurred.
func truncateTail(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return "...[truncated] " + s[len(s)-maxLen:]
}

// runCommand executes a command and returns its stdout.
// dir sets the working directory; empty means current directory.
// Override in tests to mock command execution.
var runCommand = func(ctx context.Context, name string, args []string, dir string, env []string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.Output()
	return string(out), err
}

type askRequest struct {
	Query string `json:"query"`
}

type askResponse struct {
	Answer string `json:"answer"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type healthResponse struct {
	Status string `json:"status"`
}

type refreshResponse struct {
	Status string `json:"status"`
	Output string `json:"output,omitempty"`
}

func getTimeout() time.Duration {
	if v := os.Getenv("DOCS_AGENT_TIMEOUT"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return defaultTimeout
}

func getWorkspace() string {
	if v := os.Getenv("DOCS_AGENT_WORKSPACE"); v != "" {
		return v
	}
	return defaultWorkspace
}

func getRepoDir() string {
	if v := os.Getenv("DOCS_AGENT_REPO_DIR"); v != "" {
		return v
	}
	return getWorkspace() + "/" + defaultRepoDir
}

func getSystemMD() string {
	if v := os.Getenv("GEMINI_SYSTEM_MD"); v != "" {
		return v
	}
	return getRepoDir() + "/" + defaultSystemMD
}

func getModel() string {
	if v := os.Getenv("DOCS_AGENT_MODEL"); v != "" {
		return v
	}
	return defaultModel
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

func handleAsk(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost+", "+http.MethodOptions)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 2048))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var req askRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	query := strings.TrimSpace(req.Query)
	if query == "" {
		writeError(w, http.StatusBadRequest, "query must not be empty")
		return
	}
	if len(query) > maxQueryLength {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("query must not exceed %d characters", maxQueryLength))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), getTimeout())
	defer cancel()

	model := getModel()
	repoDir := getRepoDir()
	systemMD := getSystemMD()

	args := []string{
		"--prompt", query,
		"--model", model,
	}
	env := []string{
		"GEMINI_SYSTEM_MD=" + systemMD,
	}

	debugf("ask: cd %s && GEMINI_SYSTEM_MD=%s gemini %s",
		repoDir, systemMD, shelljoin(args))

	output, err := runCommand(ctx, "gemini", args, repoDir, env)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			debugf("ask: request timed out after %s", getTimeout())
			writeError(w, http.StatusGatewayTimeout, "request timed out")
			return
		}
		// Extract stderr from exec.ExitError if available.
		stderr := ""
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = string(exitErr.Stderr)
		}
		debugf("ask: gemini command failed: err=%v stderr=%q stdout=%q", err, truncateTail(stderr, 2000), output)
		writeError(w, http.StatusInternalServerError, "failed to get response from Gemini")
		return
	}

	debugf("ask: gemini responded with %d bytes", len(output))
	answer := cleanGeminiOutput(output)
	writeJSON(w, http.StatusOK, askResponse{Answer: answer})
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	data, err := chatFS.ReadFile("chat/index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "chat widget not found")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost+", "+http.MethodOptions)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	repoDir := getRepoDir()
	debugf("refresh: running git pull in %s", repoDir)
	output, err := runCommand(ctx, "git", []string{"pull", "--ff-only"}, repoDir, nil)
	if err != nil {
		debugf("refresh: git pull failed: err=%v output=%q", err, output)
		writeError(w, http.StatusInternalServerError, "git pull failed: "+output)
		return
	}

	debugf("refresh: git pull succeeded: %s", strings.TrimSpace(output))
	writeJSON(w, http.StatusOK, refreshResponse{Status: "ok", Output: strings.TrimSpace(output)})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

// corsMiddleware wraps a handler to add CORS headers for cross-origin access.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Max-Age", "3600")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/ask", handleAsk)
	mux.HandleFunc("/chat", handleChat)
	mux.HandleFunc("/refresh", handleRefresh)
	mux.HandleFunc("/health", handleHealth)

	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	handler := corsMiddleware(mux)

	if debugEnabled() {
		log.Printf("DEBUG mode enabled")
		log.Printf("  workspace=%s", getWorkspace())
		log.Printf("  repo_dir=%s", getRepoDir())
		log.Printf("  model=%s", getModel())
		log.Printf("  system_md=%s", getSystemMD())
		log.Printf("  timeout=%s", getTimeout())
	}
	log.Printf("docs-agent listening on :%s", port)
	if err := http.ListenAndServe(":"+port, handler); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
