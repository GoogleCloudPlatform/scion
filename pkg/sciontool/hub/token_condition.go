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
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

// errTokenFileOwner is the owner check's refusal (see
// EnforceTokenFileOwnerChecks), so it can be told apart from other read
// errors without matching text.
var errTokenFileOwner = errors.New("unexpected owner")

// TokenFileCondition names the state of the agent token file without
// carrying any of its content. Each value is fixed text, safe to log.
type TokenFileCondition string

const (
	TokenFilePresent    TokenFileCondition = "present"
	TokenFileAbsent     TokenFileCondition = "absent"
	TokenFileUnreadable TokenFileCondition = "unreadable"
	TokenFileWrongOwner TokenFileCondition = "wrong owner"
	TokenFileNotRegular TokenFileCondition = "not a single-link regular file"
	TokenFileMalformed  TokenFileCondition = "malformed"
)

// CheckTokenFile reports the condition of the canonical token file
// (TokenFilePath), read through the same guarded reader as ReadTokenFile.
// It never returns or logs the value.
func CheckTokenFile() TokenFileCondition {
	token, err := readTokenFileGuarded(TokenFilePath())
	if err != nil {
		return classifyTokenFileErr(err)
	}
	if !wellFormedTokenText(token) {
		return TokenFileMalformed
	}
	return TokenFilePresent
}

func classifyTokenFileErr(err error) TokenFileCondition {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return TokenFileAbsent
	case errors.Is(err, errTokenFileOwner):
		return TokenFileWrongOwner
	case errors.Is(err, dirfd.ErrTooLarge):
		return TokenFileMalformed
	case errors.Is(err, dirfd.ErrNotSingleLinkRegular), errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENOTDIR):
		return TokenFileNotRegular
	default:
		return TokenFileUnreadable
	}
}

// wellFormedTokenText reports whether token, once trimmed, is non-empty
// printable text with no interior whitespace: the shape of every token the
// hub issues (signed credentials and the dev token alike).
func wellFormedTokenText(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	for _, r := range token {
		if unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// TokenIssuedAt returns the iat claim of a three-part token, decoding only
// that claim and checking nothing else (no signature, expiry or subject).
// ok is false for anything that is not a three-part token with a readable
// claims segment and a positive iat. Callers use it only to compare two of
// the agent's own credentials, never to trust one.
func TokenIssuedAt(token string) (iat time.Time, ok bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
		return time.Time{}, false
	}
	payload, err := decodeSegment(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Iat json.Number `json:"iat"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, false
	}
	n, err := claims.Iat.Int64()
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	return time.Unix(n, 0), true
}

// HubClientUnconfiguredReason describes, for a log line, why NewClient
// built no client although a hub endpoint is set: which required inputs
// are missing and the token file's condition. It names conditions only,
// never a value. It returns "" when nothing required is missing.
func HubClientUnconfiguredReason() string {
	var missing []string
	if os.Getenv(EnvAgentID) == "" {
		missing = append(missing, "agent ID ("+EnvAgentID+") is unset")
	}
	if cond := CheckTokenFile(); cond != TokenFilePresent && os.Getenv(EnvHubToken) == "" {
		missing = append(missing, "agent credential: token file "+string(cond)+" and no bootstrap credential")
	}
	return strings.Join(missing, "; ")
}

// RemoveTokenFile removes the canonical token file, opening every
// directory on the way without following a symbolic link and unlinking the
// leaf itself (a link planted there is removed, never followed). A file
// that is already gone is not an error. Like WriteTokenFile, it refuses to
// run under `go test` unless SetTokenHome isolated the token home.
func RemoveTokenFile() error {
	if testing.Testing() && !tokenHomeOverridden {
		panic("scion/hub: RemoveTokenFile called during a test without SetTokenHome()")
	}
	dirFd, leaf, err := dirfd.OpenParentNoFollow(TokenFilePath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = dirfd.Close(dirFd) }()
	if err := dirfd.UnlinkAt(dirFd, leaf); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
