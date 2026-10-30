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
	"fmt"
	"net"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/util/fsutil"
)

// Origin says where a project storage block came from, which decides how
// strictly ValidateProjectStorage checks it.
type Origin int

const (
	// OriginFile is a block from the host's own settings file or the
	// process environment. The operator owns the host, so mount options
	// need no allowlist (only syntax, and no suid or dev) and existing
	// share ids stay valid.
	OriginFile Origin = iota
	// OriginManaged is a block delivered by a hub for a broker to apply.
	// It adds: share ids are DNS labels, mount options come from an
	// allowlist, mount_root and auto_mount must not be set, and uid and
	// gid must be non-zero.
	OriginManaged
)

// String returns "file" or "managed".
func (o Origin) String() string {
	if o == OriginManaged {
		return "managed"
	}
	return "file"
}

// FieldError is one rule a project storage field breaks: the settings path
// of the field (for example server.project_storage.nfs.shares[0].id), the
// rule, and the offending value. Error() carries the value: use it only for
// logs and responses authorized for a hub admin. Any other response renders
// FieldErrorList.Issues with projectfs.ViewPublic, which names the fields
// only.
type FieldError struct {
	Path  string `json:"field"`
	Rule  string `json:"rule"`
	Value string `json:"value,omitempty"`
}

func (e FieldError) Error() string {
	msg := e.Rule
	if e.Path != "" && !strings.HasPrefix(e.Rule, e.Path) {
		msg = e.Path + ": " + e.Rule
	}
	if e.Value != "" {
		msg += fmt.Sprintf(" (got %q)", e.Value)
	}
	return msg
}

// FieldErrorList is a non-empty list of FieldErrors used as one error, so
// a caller can recover the fields with errors.As.
type FieldErrorList []FieldError

func (l FieldErrorList) Error() string {
	parts := make([]string, len(l))
	for i, e := range l {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "; ")
}

// Issues returns l as projectfs layout issues, for rendering with
// projectfs.RenderLayoutIssues.
func (l FieldErrorList) Issues() []projectfs.LayoutIssue {
	out := make([]projectfs.LayoutIssue, len(l))
	for i, e := range l {
		out[i] = projectfs.NewFieldIssue(e.Path, e.Rule, e.Value)
	}
	return out
}

// Paths returns the settings path of each error, in order.
func (l FieldErrorList) Paths() []string {
	out := make([]string, len(l))
	for i, e := range l {
		out[i] = e.Path
	}
	return out
}

// fieldErrorsErr returns errs as a FieldErrorList error, or nil when empty.
func fieldErrorsErr(errs []FieldError) error {
	if len(errs) == 0 {
		return nil
	}
	return FieldErrorList(errs)
}

// Limits of the project storage field rules.
const (
	maxShareIDLen   = 63
	maxExportLen    = 1024
	maxHostnameLen  = 253
	maxDNSSubdomain = 253
)

var (
	dnsLabelRe     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	dnsSubdomainRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	hostLabelRe    = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
	mountOptionRe  = regexp.MustCompile(`^[a-z0-9_]+(=[A-Za-z0-9._:/+-]+)?$`)
)

// ValidateProjectStorage checks a resolved server.project_storage block
// (defaults applied) and returns every rule it breaks; nil means valid. It
// runs the block's structural checks (Validate) first and, when they pass,
// the field rules of the backend the block selects: nfs share ids,
// servers, exports, mount options, mount_root, uid and gid, pv_name and
// storage_class, the volume names and claim names of the volume backends,
// and subpath_root. origin selects the policy (OriginFile or
// OriginManaged).
//
// It is a pure function: no I/O, and ps is not modified. Every resolution
// of the block (SynthesizeProjectStorage, and through it the hub start,
// the broker start and the admin settings writes) reaches it, and a hub
// delivering a managed block calls it with OriginManaged.
func ValidateProjectStorage(ps *V1ProjectStorageConfig, origin Origin) []FieldError {
	if ps == nil {
		return nil
	}
	if err := ps.Validate(); err != nil {
		return []FieldError{{Path: ProjectStorageKey, Rule: err.Error()}}
	}
	return validateProjectStorageFields(ps, origin, ProjectStorageKey, sharedDirStorageKey)
}

// validateProjectStorageFields implements the field rules of
// ValidateProjectStorage. key is the settings key the backend blocks are
// reported under (server.project_storage, or server.workspace_storage for a
// block synthesized from it), sdKey the key of the transitional shared-dir
// nfs block (LegacySharedDirsNFS).
func validateProjectStorageFields(ps *V1ProjectStorageConfig, origin Origin, key, sdKey string) []FieldError {
	var errs []FieldError
	switch ps.EffectiveBackend() {
	case WorkspaceStorageBackendNFS:
		errs = append(errs, validateNFSFields(ps.NFS, origin, key+".nfs", true)...)
	case WorkspaceStorageBackendCloudRunVolume:
		if v := ps.CloudRunVolume; v != nil {
			errs = append(errs, checkVolumeName(key+".cloudrun_volume.volume_name", v.VolumeName)...)
			errs = append(errs, checkSubPathRoot(key+".cloudrun_volume.subpath_root", v.SubPathRoot)...)
		}
	case WorkspaceStorageBackendGKESharedVolume:
		if v := ps.GKESharedVolume; v != nil {
			errs = append(errs, checkVolumeName(key+".gke_shared_volume.volume_name", v.VolumeName)...)
			errs = append(errs, checkSubPathRoot(key+".gke_shared_volume.subpath_root", v.SubPathRoot)...)
			errs = append(errs, checkDNSSubdomain(key+".gke_shared_volume.pv_claim_name", v.PVClaimName)...)
		}
	}
	if ps.LegacySharedDirsNFS != nil {
		// Shared dirs only: the broker never mounts this block, and its
		// mount_options and uid are ignored fields (IgnoredNFSFields).
		errs = append(errs, validateNFSFields(ps.LegacySharedDirsNFS, origin, sdKey+".nfs", false)...)
	}
	return errs
}

// validateNFSFields checks an nfs block reported under key. mounted is
// false for the shared-dir-only block, which is never mounted by the
// broker: its server and export are optional and its mount options and
// owner ids are not used.
func validateNFSFields(nfs *V1NFSConfig, origin Origin, key string, mounted bool) []FieldError {
	if nfs == nil {
		return nil
	}
	var errs []FieldError
	if origin == OriginManaged {
		if nfs.MountRoot != "" {
			errs = append(errs, FieldError{Path: key + ".mount_root", Rule: "must not be set in a managed block (the broker keeps its own)"})
		}
		if nfs.AutoMount {
			errs = append(errs, FieldError{Path: key + ".auto_mount", Rule: "must not be set in a managed block (the broker keeps its own)"})
		}
	} else {
		errs = append(errs, checkMountRoot(key+".mount_root", nfs.MountRoot)...)
	}
	seen := make(map[string]bool, len(nfs.Shares))
	for i, share := range nfs.Shares {
		p := fmt.Sprintf("%s.shares[%d]", key, i)
		errs = append(errs, checkShareID(p+".id", share.ID, origin)...)
		if share.ID != "" && seen[share.ID] {
			errs = append(errs, FieldError{Path: p + ".id", Rule: "is duplicated", Value: share.ID})
		}
		seen[share.ID] = true
		// The shared-dir-only block never mounts, so it needs no server
		// or export; when it names them they are checked all the same.
		if mounted || share.Server != "" {
			errs = append(errs, checkNFSServer(p+".server", share.Server)...)
		}
		if mounted || share.Export != "" {
			errs = append(errs, checkNFSExport(p+".export", share.Export)...)
		}
		errs = append(errs, checkDNSSubdomain(p+".pv_name", share.PVName)...)
	}
	errs = append(errs, checkDNSSubdomain(key+".storage_class", nfs.StorageClass)...)
	errs = append(errs, checkSubPathRoot(key+".subpath_root", nfs.SubPathRoot)...)
	if mounted {
		errs = append(errs, checkMountOptions(key+".mount_options", nfs.MountOptions, origin)...)
		errs = append(errs, checkOwnerID(key+".uid", nfs.UID, origin)...)
		errs = append(errs, checkOwnerID(key+".gid", nfs.GID, origin)...)
	}
	return errs
}

// checkSinglePathElement returns the rule a value joined onto a host path
// as one element breaks, or "": non-empty; no '/', '\', NUL, whitespace or
// control character; not "." or "..".
func checkSinglePathElement(v string) string {
	switch {
	case v == "":
		return "must be set"
	case v == "." || v == "..":
		return "must be a single path element, not \".\" or \"..\""
	case strings.ContainsAny(v, `/\`):
		return "must be a single path element (no path separator)"
	case strings.IndexFunc(v, func(r rune) bool { return r == 0 || unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0:
		return "must not contain whitespace, NUL or control characters"
	}
	return ""
}

func checkShareID(p, id string, origin Origin) []FieldError {
	if rule := checkSinglePathElement(id); rule != "" {
		return []FieldError{{Path: p, Rule: rule, Value: id}}
	}
	if len(id) > maxShareIDLen {
		return []FieldError{{Path: p, Rule: fmt.Sprintf("must be at most %d bytes", maxShareIDLen), Value: id}}
	}
	if origin == OriginManaged && !dnsLabelRe.MatchString(id) {
		return []FieldError{{Path: p, Rule: "must be a DNS label (lower-case letters, digits and '-', starting and ending with a letter or digit)", Value: id}}
	}
	return nil
}

func checkVolumeName(p, name string) []FieldError {
	if rule := checkSinglePathElement(name); rule != "" {
		return []FieldError{{Path: p, Rule: rule, Value: name}}
	}
	return nil
}

// allowIPv6NFSServer decides whether an unbracketed IPv6 literal is a
// valid nfs server. It is false: the broker's mount source server:export
// does not bracket it.
const allowIPv6NFSServer = false

// checkNFSServer accepts an IPv4 literal or an RFC 1123 hostname. IPv6
// literals are refused: the mount source server:export would be
// ambiguous.
func checkNFSServer(p, server string) []FieldError {
	bad := func(rule string) []FieldError { return []FieldError{{Path: p, Rule: rule, Value: server}} }
	switch {
	case server == "":
		return bad("must be set")
	case strings.HasPrefix(server, "-"):
		return bad("must not start with '-'")
	case strings.IndexFunc(server, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 ||
		strings.ContainsAny(server, ",/"):
		return bad("must not contain whitespace, control characters, ',' or '/'")
	}
	if ip := net.ParseIP(server); ip != nil && ((ip.To4() != nil && !strings.Contains(server, ":")) || allowIPv6NFSServer) {
		return nil
	}
	if strings.ContainsAny(server, ":[]") {
		return bad("must be a hostname or an IPv4 address (IPv6 literals are not supported)")
	}
	if len(server) > maxHostnameLen {
		return bad(fmt.Sprintf("must be an IP address or a hostname of at most %d bytes", maxHostnameLen))
	}
	for _, label := range strings.Split(server, ".") {
		if !hostLabelRe.MatchString(label) {
			return bad("must be an IPv4 address or an RFC 1123 hostname")
		}
	}
	return nil
}

// checkNFSExport accepts an absolute, path.Clean-stable path with no ".."
// component, whitespace, ',' or NUL, of at most maxExportLen bytes.
func checkNFSExport(p, export string) []FieldError {
	bad := func(rule string) []FieldError { return []FieldError{{Path: p, Rule: rule, Value: export}} }
	switch {
	case export == "":
		return bad("must be set")
	case len(export) > maxExportLen:
		return bad(fmt.Sprintf("must be at most %d bytes", maxExportLen))
	case !strings.HasPrefix(export, "/"):
		return bad("must be an absolute path")
	case strings.IndexFunc(export, func(r rune) bool { return r == 0 || unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 ||
		strings.Contains(export, ","):
		return bad("must not contain whitespace, control characters, NUL or ','")
	case pathHasDotDot(export):
		return bad("must not contain a \"..\" path component")
	case path.Clean(export) != export:
		return bad(fmt.Sprintf("must be a clean path (use %q)", path.Clean(export)))
	}
	return nil
}

func checkMountRoot(p, root string) []FieldError {
	bad := func(rule string) []FieldError { return []FieldError{{Path: p, Rule: rule, Value: root}} }
	switch {
	case root == "":
		return bad("must be set to an absolute path")
	case !filepath.IsAbs(root) || !strings.HasPrefix(root, "/"):
		return bad("must be an absolute path")
	case strings.IndexFunc(root, func(r rune) bool { return r == 0 || unicode.IsControl(r) }) >= 0:
		return bad("must not contain NUL or control characters")
	case pathHasDotDot(root):
		return bad("must not contain a \"..\" path component")
	case path.Clean(root) != root:
		return bad(fmt.Sprintf("must be a clean path (use %q)", path.Clean(root)))
	}
	return nil
}

func pathHasDotDot(p string) bool {
	for _, c := range strings.Split(p, "/") {
		if c == ".." {
			return true
		}
	}
	return false
}

func checkSubPathRoot(p, root string) []FieldError {
	if err := ValidateSubPathRoot(SubPathRootOrDefault(root)); err != nil {
		return []FieldError{{Path: p, Rule: err.Error()}}
	}
	return nil
}

// checkDNSSubdomain accepts "" (unset) or a DNS-1123 subdomain of at most
// 253 bytes.
func checkDNSSubdomain(p, v string) []FieldError {
	if v == "" {
		return nil
	}
	if len(v) > maxDNSSubdomain || !dnsSubdomainRe.MatchString(v) {
		return []FieldError{{Path: p, Rule: "must be a DNS-1123 subdomain (lower-case letters, digits, '-' and '.', at most 253 bytes)", Value: v}}
	}
	return nil
}

func checkOwnerID(p string, id int, origin Origin) []FieldError {
	if err := fsutil.ValidateOwnerID(p, id); err != nil {
		return []FieldError{{Path: p, Rule: err.Error()}}
	}
	if origin == OriginManaged && id == 0 {
		return []FieldError{{Path: p, Rule: "must not be 0 in a managed block"}}
	}
	return nil
}

// checkMountOptions checks the comma-separated mount_options. Every
// origin: no empty token; each token is name or name=value matching
// mountOptionRe (so no leading '-', whitespace or shell syntax); and no
// token re-enables what the broker's mounts turn off (refusedMountOption).
// Managed blocks: each token is also in managedMountOptions. Mount options
// are hardened separately.
func checkMountOptions(p, opts string, origin Origin) []FieldError {
	if opts == "" {
		return nil
	}
	for _, tok := range strings.Split(opts, ",") {
		if tok == "" {
			return []FieldError{{Path: p, Rule: "must not contain an empty option", Value: opts}}
		}
		if !mountOptionRe.MatchString(tok) {
			return []FieldError{{Path: p, Rule: fmt.Sprintf("option %q must be name or name=value (lower-case name; value of letters, digits and ._:/+-)", tok), Value: opts}}
		}
		if refusedMountOption(tok) {
			return []FieldError{{Path: p, Rule: fmt.Sprintf("option %q is not allowed (it contradicts the nosuid,nodev every mount carries, or gives a flag a value)", tok), Value: opts}}
		}
		if origin == OriginManaged && !managedMountOptionAllowed(tok) {
			return []FieldError{{Path: p, Rule: fmt.Sprintf("option %q is not allowed in a managed block", tok), Value: opts}}
		}
	}
	return nil
}

// refusedMountOption reports a token no origin may set: suid and dev in
// any form, which contradict the nosuid,nodev every broker mount carries,
// and a valued form of the flags nosuid, nodev and rw. Other options, such
// as exec or defaults, are left to the operator in a file block; the
// options the broker enforces are appended after them.
func refusedMountOption(tok string) bool {
	name, _, hasValue := strings.Cut(tok, "=")
	switch name {
	case "suid", "dev":
		return true
	case "nosuid", "nodev", "rw":
		return hasValue
	}
	return false
}

// managedMountOptions is the allowlist of mount options a managed block
// may carry: the name, and a check of the value ("" for a flag, which
// takes none). Everything else is refused. nosuid, nodev and rw are
// accepted as no-ops.
var managedMountOptions = map[string]func(v string) bool{
	"vers":        oneOf("3", "4", "4.0", "4.1", "4.2"),
	"nfsvers":     oneOf("3", "4", "4.0", "4.1", "4.2"),
	"proto":       oneOf("tcp"),
	"hard":        nil,
	"nconnect":    intIn(1, 16),
	"rsize":       intIn(4096, 1048576),
	"wsize":       intIn(4096, 1048576),
	"timeo":       intIn(1, 600),
	"retrans":     intIn(0, 10),
	"actimeo":     intIn(0, 60),
	"noatime":     nil,
	"nodiratime":  nil,
	"_netdev":     nil,
	"lookupcache": oneOf("all", "none", "pos"),
	"nolock":      nil,
	"local_lock":  oneOf("none", "all"),
	"nosuid":      nil,
	"nodev":       nil,
	"rw":          nil,
}

func managedMountOptionAllowed(tok string) bool {
	name, value, hasValue := strings.Cut(tok, "=")
	check, ok := managedMountOptions[name]
	if !ok {
		return false
	}
	if check == nil {
		return !hasValue
	}
	return hasValue && check(value)
}

func oneOf(values ...string) func(string) bool {
	return func(v string) bool {
		for _, want := range values {
			if v == want {
				return true
			}
		}
		return false
	}
}

func intIn(lo, hi int) func(string) bool {
	return func(v string) bool {
		n, err := strconv.Atoi(v)
		return err == nil && strconv.Itoa(n) == v && n >= lo && n <= hi
	}
}
