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

package runtimebroker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Delivery of attachments into the agent's home (ptone/scion#4596, design
// section 8.4).
//
// When a message request carries attachments, the broker starts redeeming
// their capabilities at receipt, on a context detached from the request
// and bounded by inboxRedeemTimeout, into a broker-local spool, and checks
// each file's size and sha256. Immediately before the message text is
// delivered, it waits for the redemption (within the same bound), writes
// the verified files into ~/.scion/inbox/<message-id>/ with one exec as the
// agent user (a tar stream on stdin, extracted into a temporary directory,
// then an atomic claim and a rename), and appends one line per file that
// did not land after the message's text. The hub-rendered text is never
// edited.

// inboxRedeemTimeout bounds the redemption of one message's attachments,
// counted from receipt. A variable so tests can shorten it.
var inboxRedeemTimeout = 60 * time.Second

const (
	// inboxWriteTimeout bounds the in-container write of one message.
	inboxWriteTimeout = 60 * time.Second
	// substrateChunkBytes is the most a Substrate exec carries on stdin
	// per chunk; the runtime caps stdin at about 783 KB.
	substrateChunkBytes = 512 << 10
	// substrateChunkTimeout is the extra write time per Substrate chunk;
	// each Substrate exec also runs a probe exec.
	substrateChunkTimeout = 4 * time.Second
	// inboxPruneInterval is how often, at most, one agent's inbox is
	// pruned (after a successful write).
	inboxPruneInterval = time.Hour
	// defaultInboxRetentionDays is used when the hub sends no retention.
	defaultInboxRetentionDays = 7
	// inboxAgentUser is the agent user the write runs as.
	inboxAgentUser = "scion"
)

// Reasons of a file that did not land, as the agent reads them.
const (
	reasonNotDownloaded = messaging.AttachmentReasonNotDownloaded
	reasonVerification  = messaging.AttachmentReasonVerification
	reasonNotWritten    = messaging.AttachmentReasonNotWritten
)

// inboxFetcherOverride, when set, is the fetcher every delivery redeems
// with. Tests set it.
var inboxFetcherOverride hubclient.AttachmentDeliveryFetcher

// inboxState is the broker's attachment delivery state.
type inboxState struct {
	spool     string
	pruneMu   sync.Mutex
	lastPrune map[string]time.Time
}

// inboxIDPattern matches a message id usable in an inbox path (the hub
// sends UUIDs).
var inboxIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// inboxNamePattern matches a hub-reduced attachment name.
var inboxNamePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,119}$`)

// initInboxSpool sets the spool root under the broker state directory and
// removes spools a crash left behind. Without a state directory the spool
// is a new private temporary directory (os.MkdirTemp, mode 0700), so no
// predictable path is ever reused. It runs before the broker accepts
// messages.
func (s *Server) initInboxSpool() {
	if s.stateDir == "" {
		root, err := os.MkdirTemp("", "scion-inbox-spool-")
		if err != nil {
			slog.Warn("Failed to create the attachment spool directory", "error", err)
			return
		}
		s.inbox.spool = root
		s.inbox.lastPrune = map[string]time.Time{}
		return
	}
	root := filepath.Join(s.stateDir, "inbox-spool")
	if err := os.RemoveAll(root); err != nil {
		slog.Warn("Failed to remove leftover attachment spools", "dir", root, "error", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		slog.Warn("Failed to create the attachment spool directory", "dir", root, "error", err)
		return
	}
	s.inbox.spool = root
	s.inbox.lastPrune = map[string]time.Time{}
}

// inboxFile is the state of one attachment of a delivery.
type inboxFile struct {
	wire     messages.WireAttachment
	spooled  string // verified file in the spool, "" until then
	reason   string // why it did not land, "" while it may
	finished bool
}

// inboxDelivery is the attachment delivery of one message to one agent.
// It implements agent.PreDelivery.
type inboxDelivery struct {
	s           *Server
	connName    string
	agentID     string
	messageID   string
	retention   int
	spool       string
	started     time.Time
	cancel      context.CancelFunc
	done        chan struct{}
	mu          sync.Mutex
	files       []*inboxFile
	cleanupOnce sync.Once
}

// startInboxDelivery starts redeeming the attachments of a message to
// agentID, at receipt, on a context detached from the request. It returns
// nil when there is nothing to deliver. Entries the hub could not issue a
// capability for, and any delivery the broker cannot spool, are reported
// as not delivered.
func (s *Server) startInboxDelivery(connName, agentID, messageID string, retention int, wire []messages.WireAttachment) *inboxDelivery {
	if len(wire) == 0 {
		return nil
	}
	d := &inboxDelivery{s: s, connName: connName, agentID: agentID, messageID: messageID,
		retention: retention, started: time.Now(), done: make(chan struct{})}
	for _, w := range wire {
		d.files = append(d.files, &inboxFile{wire: w})
	}
	ctx, cancel := context.WithTimeout(context.Background(), inboxRedeemTimeout)
	d.cancel = cancel
	if s.inbox.spool == "" || !inboxIDPattern.MatchString(messageID) {
		d.failAll(reasonNotDownloaded)
		close(d.done)
		return d
	}
	dir, err := os.MkdirTemp(s.inbox.spool, messageID+"-")
	if err != nil {
		d.failAll(reasonNotDownloaded)
		close(d.done)
		return d
	}
	d.spool = dir
	fetch := d.fetcher()
	go func() {
		defer close(d.done)
		defer cancel()
		d.redeemAll(ctx, fetch)
	}()
	return d
}

// failAll marks every unfinished file as not delivered for reason.
func (d *inboxDelivery) failAll(reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, f := range d.files {
		if !f.finished {
			f.reason, f.finished = reason, true
		}
	}
}

// fetcher returns the hub client to redeem with: the connection the
// message arrived on, else any connection whose client can redeem.
func (d *inboxDelivery) fetcher() hubclient.AttachmentDeliveryFetcher {
	if inboxFetcherOverride != nil {
		return inboxFetcherOverride
	}
	s := d.s
	s.hubMu.RLock()
	defer s.hubMu.RUnlock()
	if conn, ok := s.hubConnections[d.connName]; d.connName != "" && ok && conn.HubClient != nil {
		if f, ok := conn.HubClient.(hubclient.AttachmentDeliveryFetcher); ok {
			return f
		}
	}
	for _, conn := range s.hubConnections {
		if conn.HubClient == nil {
			continue
		}
		if f, ok := conn.HubClient.(hubclient.AttachmentDeliveryFetcher); ok {
			return f
		}
	}
	return nil
}

// redeemAll downloads and verifies each file in turn.
func (d *inboxDelivery) redeemAll(ctx context.Context, fetch hubclient.AttachmentDeliveryFetcher) {
	for _, f := range d.files {
		reason := ""
		spooled := ""
		switch {
		case f.wire.URL == "" || fetch == nil || !inboxNamePattern.MatchString(f.wire.Name):
			reason = reasonNotDownloaded
		default:
			spooled, reason = d.redeemOne(ctx, fetch, f.wire)
		}
		d.mu.Lock()
		f.spooled, f.reason, f.finished = spooled, reason, true
		d.mu.Unlock()
	}
}

// redeemOne downloads one file into the spool and verifies its size and
// sha256. A file that fails a check is removed from the spool.
func (d *inboxDelivery) redeemOne(ctx context.Context, fetch hubclient.AttachmentDeliveryFetcher, w messages.WireAttachment) (string, string) {
	body, err := fetch.FetchAttachmentDelivery(ctx, w.URL)
	if err != nil {
		return "", reasonNotDownloaded
	}
	defer func() { _ = body.Close() }()
	path := filepath.Join(d.spool, w.Name)
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", reasonNotDownloaded
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(body, w.Size+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return "", reasonNotDownloaded
	}
	if n != w.Size || !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), w.SHA256) {
		_ = os.Remove(path)
		return "", reasonVerification
	}
	return path, ""
}

// wait waits for the redemption to finish, at most until the redemption
// bound counted from receipt; files still downloading then are reported as
// not downloaded.
func (d *inboxDelivery) wait(ctx context.Context) {
	remaining := time.Until(d.started.Add(inboxRedeemTimeout))
	timer := time.NewTimer(max(remaining, 0))
	defer timer.Stop()
	select {
	case <-d.done:
		return
	case <-timer.C:
	case <-ctx.Done():
	}
	d.cancel()
	<-d.done
	d.failAll(reasonNotDownloaded)
}

// BeforeText implements agent.PreDelivery: it waits for the redemption,
// writes the verified files into the agent's home through rt and returns
// one line per file that did not land.
func (d *inboxDelivery) BeforeText(ctx context.Context, rt scionrt.Runtime, containerID string) string {
	defer d.Discard()
	d.wait(ctx)
	var ready []*inboxFile
	d.mu.Lock()
	for _, f := range d.files {
		if f.reason == "" && f.spooled != "" {
			ready = append(ready, f)
		}
	}
	d.mu.Unlock()
	if len(ready) > 0 {
		if err := d.write(ctx, rt, containerID, ready); err != nil {
			slog.Warn("attachment delivery: write into the agent home failed",
				"agent_id", d.agentID, "message_id", d.messageID, "error", err)
			d.mu.Lock()
			for _, f := range ready {
				f.reason = reasonNotWritten
			}
			d.mu.Unlock()
		} else {
			// Off the delivery path: the text does not wait for it.
			go d.s.maybePruneInbox(rt, containerID, d.agentID, d.retention)
		}
	}
	d.report()
	return d.notDeliveredText()
}

// notDeliveredText renders the lines of the files that did not land,
// after a blank line, or "".
func (d *inboxDelivery) notDeliveredText() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var lines []string
	for _, f := range d.files {
		if f.reason == "" {
			continue
		}
		path := f.wire.Path
		if path == "" || strings.ContainsAny(path, "\n\r") {
			path = f.wire.Name
		}
		lines = append(lines, messaging.AttachmentNotDeliveredLine(path, f.wire.Size, f.reason))
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n\n" + strings.Join(lines, "\n")
}

// Discard implements agent.PreDelivery: it stops the redemption and
// removes the spool.
func (d *inboxDelivery) Discard() {
	d.cleanupOnce.Do(func() {
		d.cancel()
		<-d.done
		if d.spool != "" {
			_ = os.RemoveAll(d.spool)
		}
	})
}

// report sends the per-file outcomes to the hub.
func (d *inboxDelivery) report() {
	d.mu.Lock()
	outcomes := make([]hubclient.AttachmentOutcome, 0, len(d.files))
	for _, f := range d.files {
		o := hubclient.AttachmentOutcome{MessageID: d.messageID, AgentID: d.agentID, AttachmentID: f.wire.ID, Outcome: "delivered"}
		if f.reason != "" {
			o.Outcome, o.Reason = "not_delivered", f.reason
		}
		outcomes = append(outcomes, o)
	}
	d.mu.Unlock()
	go d.s.reportAttachmentOutcomes(d.connName, outcomes)
}

// inboxScriptPath gives every inbox script a PATH: the Cloud Run sandbox
// execs with an empty one.
const inboxScriptPath = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin; export PATH\n"

// inboxWriteScript is the shell script that writes one message's files into
// the agent's home: $1 is the message id, $2 a random suffix; the tar
// stream comes on stdin, or, with $3, from that file in the inbox (a
// Substrate upload). It never follows a symlink on the inbox path, creates
// directories 0700 with umask 077, extracts into a temporary directory and
// moves it into place only under an atomic claim, so a repeated or
// concurrent delivery of the same message never nests. The claim is a file
// created by the shell's noclobber redirection (O_EXCL in dash, bash and
// busybox ash), not by mkdir(1): some mkdir implementations report success
// when they lose the race to a directory created concurrently (uutils
// coreutils 0.8 to 0.10 do; 0.11.0 fixed it, but hosts and images may
// still ship an affected version), which let two writers move. An existing
// claim of any type, including a directory claim from an older broker,
// holds the message. Until the move succeeds, the exit trap removes the
// claim with the temporary directory, so a failed write leaves no claim. A
// message directory that already exists counts as delivered.
const inboxWriteScript = inboxScriptPath + `set -eu
umask 077
home="${HOME:?}"
inbox="$home/.scion/inbox"
for d in "$home/.scion" "$inbox"; do
  if [ -L "$d" ]; then echo "symlink on the inbox path" >&2; exit 3; fi
done
mkdir -p "$inbox"
chmod 700 "$inbox"
tmp="$inbox/.tmp-$1-$2"
src="${3:-}"
claim="$inbox/.claim-$1"
claimed=""
cleanup() {
  rm -rf "$tmp"
  if [ -n "$src" ]; then rm -f "$inbox/$src"; fi
  if [ -n "$claimed" ]; then rm -f "$claim"; fi
}
mkdir -m 700 "$tmp"
trap cleanup EXIT
if [ -n "$src" ]; then
  tar -x -C "$tmp" -f "$inbox/$src"
else
  tar -x -C "$tmp" -f -
fi
if (set -C; : > "$claim") 2>/dev/null; then
  claimed=1
  if [ ! -e "$inbox/$1" ]; then
    mv "$tmp" "$inbox/$1"
  fi
  claimed=""
fi
exit 0
`

// inboxCommand returns the exec vector that runs script with args as the
// agent user on runtime rt. The Cloud Run sandbox's exec already runs as
// the agent (uid 0 under gVisor) with an empty PATH, so it gets an absolute
// shell; every other runtime goes through ExecAsUserCmd, which execs
// directly when the exec user already is the agent and switches to it
// otherwise (the Cloud Run connector runs as root).
func inboxCommand(rt scionrt.Runtime, script string, args ...string) []string {
	if rt.Name() == "cloudrun-sandbox" {
		return append([]string{"/bin/sh", "-c", script, "scion-inbox"}, args...)
	}
	cmd := "sh -c " + shellQuote(script) + " scion-inbox"
	for _, a := range args {
		cmd += " " + shellQuote(a)
	}
	return scionrt.ExecAsUserCmd(inboxAgentUser, cmd)
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// write writes the verified files into the agent's home with one exec. The
// tar is streamed from the spool on stdin; a Substrate actor gets it in
// chunks first, so it is built in memory there.
func (d *inboxDelivery) write(ctx context.Context, rt scionrt.Runtime, containerID string, files []*inboxFile) error {
	if rt == nil || containerID == "" {
		return fmt.Errorf("no running container")
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	rnd := hex.EncodeToString(suffix)
	if rt.Name() == "substrate" {
		var buf bytes.Buffer
		if err := writeInboxTar(&buf, files); err != nil {
			return err
		}
		return d.writeChunked(ctx, rt, containerID, buf.Bytes(), rnd)
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), inboxWriteTimeout)
	defer cancel()
	pr, pw := io.Pipe()
	tarDone := make(chan error, 1)
	go func() {
		err := writeInboxTar(pw, files)
		_ = pw.CloseWithError(err)
		tarDone <- err
	}()
	_, err := rt.ExecWithStdin(wctx, containerID, inboxCommand(rt, inboxWriteScript, d.messageID, rnd), pr)
	// Unblock the tar writer if the exec stopped reading.
	_ = pr.CloseWithError(io.ErrClosedPipe)
	tarErr := <-tarDone
	if err != nil {
		return err
	}
	if tarErr != nil && !errors.Is(tarErr, io.ErrClosedPipe) {
		return tarErr
	}
	return nil
}

// writeInboxTar writes a tar of the verified spool files to w, each under
// its hub-reduced name with mode 0600.
func writeInboxTar(w io.Writer, files []*inboxFile) error {
	tw := tar.NewWriter(w)
	for _, f := range files {
		if err := appendInboxTarFile(tw, f); err != nil {
			return err
		}
	}
	return tw.Close()
}

func appendInboxTarFile(tw *tar.Writer, f *inboxFile) error {
	file, err := os.Open(f.spooled)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: f.wire.Name, Mode: 0o600, Size: info.Size(),
		Typeflag: tar.TypeReg, ModTime: time.Now()}); err != nil {
		return err
	}
	_, err = io.CopyN(tw, file, info.Size())
	return err
}

// inboxAppendScript appends stdin to the upload file $1 in the inbox,
// creating the inbox (0700, no symlinks) on the first chunk.
const inboxAppendScript = inboxScriptPath + `set -eu
umask 077
home="${HOME:?}"
inbox="$home/.scion/inbox"
for d in "$home/.scion" "$inbox"; do
  if [ -L "$d" ]; then exit 3; fi
done
mkdir -p "$inbox"
chmod 700 "$inbox"
cat >> "$inbox/$1"
`

// writeChunked sends the tar to a Substrate actor in chunks of at most
// substrateChunkBytes appended to an upload file, then extracts it.
func (d *inboxDelivery) writeChunked(ctx context.Context, rt scionrt.Runtime, containerID string, data []byte, rnd string) error {
	chunks := (len(data) + substrateChunkBytes - 1) / substrateChunkBytes
	timeout := inboxWriteTimeout + time.Duration(chunks)*substrateChunkTimeout
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	upload := ".upload-" + d.messageID + "-" + rnd + ".tar"
	for off := 0; off < len(data); off += substrateChunkBytes {
		end := min(off+substrateChunkBytes, len(data))
		if _, err := rt.ExecWithStdin(wctx, containerID, inboxCommand(rt, inboxAppendScript, upload), bytes.NewReader(data[off:end])); err != nil {
			return err
		}
	}
	_, err := rt.ExecWithStdin(wctx, containerID, inboxCommand(rt, inboxWriteScript, d.messageID, rnd, upload), bytes.NewReader(nil))
	return err
}

// inboxPruneScript removes message directories older than $1 days from the
// agent's inbox, with their claims, plus claims (files, or directories from
// an older broker) without a message directory and temporary directories
// older than an hour. It touches
// nothing outside the inbox, follows no symlink (find without -L; rm -rf
// does not follow links inside a directory) and skips top-level .nfs files.
// A removal that fails (an NFS file still open inside an old directory)
// does not stop the other sweeps.
const inboxPruneScript = inboxScriptPath + `set -eu
inbox="${HOME:?}/.scion/inbox"
if [ ! -d "$inbox" ] || [ -L "$inbox" ] || [ -L "${HOME}/.scion" ]; then exit 0; fi
find "$inbox" -mindepth 1 -maxdepth 1 -type d ! -name '.*' -mtime +"$1" | while read -r d; do
  n="${d##*/}"
  rm -rf -- "$inbox/$n" "$inbox/.claim-$n" || true
done
find "$inbox" -mindepth 1 -maxdepth 1 \( -type f -o -type d \) -name '.claim-*' -mmin +60 | while read -r c; do
  n="${c##*/.claim-}"
  if [ ! -e "$inbox/$n" ]; then rm -f -- "$c" 2>/dev/null || rmdir -- "$c" 2>/dev/null || true; fi
done
find "$inbox" -mindepth 1 -maxdepth 1 -type d -name '.tmp-*' -mmin +60 -exec rm -rf -- {} + || true
find "$inbox" -mindepth 1 -maxdepth 1 -type f -name '.upload-*' -mmin +60 -exec rm -f -- {} + || true
`

// maybePruneInbox prunes agentID's inbox after a successful write, at most
// once per inboxPruneInterval (an in-memory timestamp per agent).
func (s *Server) maybePruneInbox(rt scionrt.Runtime, containerID, agentID string, retention int) {
	if retention < 1 {
		retention = defaultInboxRetentionDays
	}
	s.inbox.pruneMu.Lock()
	if s.inbox.lastPrune == nil {
		s.inbox.lastPrune = map[string]time.Time{}
	}
	if last, ok := s.inbox.lastPrune[agentID]; ok && time.Since(last) < inboxPruneInterval {
		s.inbox.pruneMu.Unlock()
		return
	}
	s.inbox.lastPrune[agentID] = time.Now()
	s.inbox.pruneMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), inboxWriteTimeout)
	defer cancel()
	if _, err := rt.ExecWithStdin(ctx, containerID, inboxCommand(rt, inboxPruneScript, strconv.Itoa(retention)), bytes.NewReader(nil)); err != nil {
		slog.Warn("attachment inbox prune failed", "agent_id", agentID, "error", err)
	}
}
