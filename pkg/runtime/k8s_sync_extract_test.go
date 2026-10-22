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

package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/syncarchive"
)

type syncEntry struct {
	name, body, link string
	typ              byte
}

// podArchive builds the gzip-compressed tar stream a pod would send, with
// padding after the entries so the stream is larger than one pipe write.
func podArchive(t *testing.T, entries ...syncEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Linkname: e.link, ModTime: time.Unix(0, 0)}
		if e.typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if e.typ == tar.TypeDir {
			hdr.Mode = 0o755
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// streamOf returns a stream function that writes b in small chunks, as the
// exec stream does, and returns the first write error.
func streamOf(b []byte) func(io.Writer) error {
	return func(w io.Writer) error {
		for len(b) > 0 {
			n := min(len(b), 512)
			if _, err := w.Write(b[:n]); err != nil {
				return err
			}
			b = b[n:]
		}
		return nil
	}
}

func syncDest(t *testing.T) (dest, outside string) {
	t.Helper()
	base := t.TempDir()
	dest = filepath.Join(base, "ws")
	outside = filepath.Join(base, "outside")
	for p, body := range map[string]string{
		filepath.Join(dest, ".scion", "settings.yaml"): "broker",
		filepath.Join(outside, "victim.txt"):           "untouched",
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dest, outside
}

func requireContent(t *testing.T, p, want string) {
	t.Helper()
	if b, err := os.ReadFile(p); err != nil || string(b) != want {
		t.Errorf("%s = %q (%v), want %q", p, b, err, want)
	}
}

// A workspace archive from the pod updates the broker's copy but leaves
// its .scion alone, and links leaving the workspace are not written.
func TestExtractPodArchive_RegularTree(t *testing.T) {
	dest, outside := syncDest(t)
	archive := podArchive(t,
		syncEntry{name: "./", typ: tar.TypeDir},
		syncEntry{name: "./src/", typ: tar.TypeDir},
		syncEntry{name: "./src/main.go", body: "package main", typ: tar.TypeReg},
		syncEntry{name: "./AGENTS.md", body: "agents", typ: tar.TypeReg},
		syncEntry{name: "./claude.md", link: "AGENTS.md", typ: tar.TypeSymlink},
		syncEntry{name: "./out", link: "../outside/victim.txt", typ: tar.TypeSymlink},
		syncEntry{name: "./.scion/settings.yaml", body: "agent", typ: tar.TypeReg},
		syncEntry{name: "./.scion/agents/a/prompt.md", body: "agent", typ: tar.TypeReg},
	)
	res, err := extractPodArchive(dest, []string{sharedWorkspaceAgentsDir}, streamOf(archive), nil)
	if err != nil {
		t.Fatal(err)
	}
	requireContent(t, filepath.Join(dest, "src", "main.go"), "package main")
	requireContent(t, filepath.Join(dest, ".scion", "settings.yaml"), "broker")
	requireContent(t, filepath.Join(outside, "victim.txt"), "untouched")
	if target, err := os.Readlink(filepath.Join(dest, "claude.md")); err != nil || target != "AGENTS.md" {
		t.Errorf("claude.md -> %q (%v)", target, err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "out")); !os.IsNotExist(err) {
		t.Errorf("link leaving the workspace written (err %v)", err)
	}
	if _, err := os.Lstat(filepath.Join(dest, ".scion", "agents")); !os.IsNotExist(err) {
		t.Errorf(".scion/agents written (err %v)", err)
	}
	if res.Files != 2 || res.Protected != 2 || res.UnsafeCount != 1 {
		t.Errorf("result = %+v", res)
	}
}

// An archive with an entry leaving the workspace is refused; the stream is
// cut off instead of blocking, and nothing outside the workspace changes.
func TestExtractPodArchive_RefusedArchiveStopsStream(t *testing.T) {
	dest, outside := syncDest(t)
	// Incompressible, so the stream is far larger than what the extractor
	// reads before refusing the first entry.
	big := make([]byte, 1<<20)
	_, _ = rand.NewChaCha8([32]byte{1}).Read(big)
	archive := podArchive(t,
		syncEntry{name: "../outside/victim.txt", body: "evil", typ: tar.TypeReg},
		syncEntry{name: "./after.bin", body: string(big), typ: tar.TypeReg},
	)
	var streamErr error
	stream := func(w io.Writer) error {
		streamErr = streamOf(archive)(w)
		return streamErr
	}
	done := make(chan error, 1)
	go func() {
		_, err := extractPodArchive(dest, nil, stream, nil)
		done <- err
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("extraction did not return; the stream is blocked")
	}
	if !errors.Is(err, syncarchive.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if streamErr == nil {
		t.Error("stream was not cut off")
	}
	requireContent(t, filepath.Join(outside, "victim.txt"), "untouched")
	if _, err := os.Lstat(filepath.Join(dest, "after.bin")); !os.IsNotExist(err) {
		t.Errorf("entry after the refused one written (err %v)", err)
	}
}

// A failed stream is reported with the remote stderr, and a file cut off
// by it is not left half written.
func TestExtractPodArchive_StreamFailure(t *testing.T) {
	dest, _ := syncDest(t)
	archive := podArchive(t,
		syncEntry{name: "./big.txt", body: strings.Repeat("y", 1<<20), typ: tar.TypeReg},
	)
	stderr := bytes.NewBufferString("tar: something broke")
	stream := func(w io.Writer) error {
		_ = streamOf(archive[:len(archive)/2])(w)
		return errors.New("command terminated with exit code 2")
	}
	_, err := extractPodArchive(dest, nil, stream, stderr)
	if err == nil || !strings.Contains(err.Error(), "stream failed") || !strings.Contains(err.Error(), "something broke") {
		t.Fatalf("err = %v, want the stream failure with remote stderr", err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "big.txt")); !os.IsNotExist(err) {
		t.Errorf("partial file left (err %v)", err)
	}
}

// A refused archive is not retried, whatever its entry names say.
func TestIsSyncTransientError_RefusedArchive(t *testing.T) {
	dest, _ := syncDest(t)
	archive := podArchive(t, syncEntry{name: "../connection reset timeout EOF", body: "x", typ: tar.TypeReg})
	_, err := extractPodArchive(dest, nil, streamOf(archive), nil)
	if !errors.Is(err, syncarchive.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if isSyncTransientError(err) {
		t.Errorf("isSyncTransientError(%v) = true", err)
	}
}
