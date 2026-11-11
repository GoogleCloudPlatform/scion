# WebDAV PROPFIND Checksum Support

> **Superseded (ptone/scion#5547):** this was never built, and the hub WebDAV endpoint it would extend has been removed, together with project-level `scion sync` and `pkg/projectsync`. Kept as history only.

**Created:** 2026-04-01
**Status:** Planned
**Related:** `workspace-sync.md`, `pkg/hub/grove_webdav.go`, `pkg/grovesync/grovesync.go`

---

## 1. Problem

rclone's WebDAV backend does not use standard ETags for file comparison. It only
recognises vendor-specific checksum properties:

- **OwnCloud/Nextcloud:** `oc:checksums` in namespace `http://owncloud.org/ns`
- **Fastmail:** `ME:sha1hex`

Our WebDAV server (Go's `golang.org/x/net/webdav`) returns ETags but no
vendor-specific properties, so rclone reports zero hash support. This causes:

1. `--checksum` mode falls back to modtime comparison
2. modtime isn't reliably supported by the WebDAV backend either
3. rclone emits noisy warnings on every sync operation

The current workaround (`ci.CheckSum = true`) is ineffective.

---

## 2. Solution

Add OwnCloud-style `oc:checksums` properties to PROPFIND responses on the
server side, and configure the rclone client to use `vendor=owncloud`.

### 2.1 Why OwnCloud Format

- rclone has mature, well-tested support for it
- Simple space-separated `ALGO:hash` format
- Enables both SHA1 and MD5 comparison

---

## 3. Server-Side Changes (`pkg/hub/grove_webdav.go`)

### 3.1 DeadPropsHolder Implementation

Go's `webdav.File` interface supports an optional `DeadPropsHolder` interface.
If a `File` implements it, the webdav handler automatically includes the
returned properties in PROPFIND responses. No handler patching required.

```go
type DeadPropsHolder interface {
    DeadProps() (map[xml.Name]Property, error)
    Patch([]Proppatch) ([]Propstat, error)
}
```

**New type: `checksumFile`** — wraps `webdav.File` to add checksum properties.

```go
type checksumFile struct {
    webdav.File
    rootPath string   // workspace root for resolving full path
    relPath  string   // path relative to workspace root
}
```

`DeadProps()` returns a single property:

```xml
<oc:checksums xmlns:oc="http://owncloud.org/ns">
  <oc:checksum>SHA1:abcdef123456...</oc:checksum>
</oc:checksums>
```

`Patch()` returns 403 Forbidden for all attempts (checksums are computed, not
stored).

### 3.2 Integration with `filteredFS`

The existing `filteredFS.OpenFile()` already wraps directory files with
`filteredDir`. Extend this to also wrap regular files with `checksumFile`:

```go
func (fs *filteredFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
    // ... existing exclusion check ...
    f, err := fs.root.OpenFile(ctx, name, flag, perm)
    if err != nil {
        return f, err
    }

    info, statErr := f.Stat()
    if statErr == nil {
        if info.IsDir() {
            return &filteredDir{File: f, dirName: name}, nil
        }
        return &checksumFile{File: f, rootPath: fs.rootPath, relPath: name}, nil
    }
    return f, nil
}
```

### 3.3 Checksum Computation Strategy

**Phase 1 (MVP): Compute on-the-fly during PROPFIND.**

For grove config directories (`.scion/` contents), files are small and few.
Computing SHA1 on each PROPFIND is acceptable. Implementation:

```go
func (f *checksumFile) computeSHA1() (string, error) {
    fullPath := filepath.Join(f.rootPath, f.relPath)
    file, err := os.Open(fullPath)
    if err != nil {
        return "", err
    }
    defer file.Close()

    h := sha1.New()
    if _, err := io.Copy(h, file); err != nil {
        return "", err
    }
    return fmt.Sprintf("%x", h.Sum(nil)), nil
}
```

**Phase 2 (If needed): Cache checksums in the hub store.**

If workspaces grow large enough that on-the-fly computation becomes a
bottleneck, add a `file_checksums` table to the hub store:

| Column       | Type   | Description                     |
|-------------|--------|---------------------------------|
| grove_id    | TEXT   | FK to grove                     |
| file_path   | TEXT   | Relative path within workspace  |
| sha1        | TEXT   | Hex-encoded SHA1                |
| size        | INT    | File size at computation time   |
| computed_at | TIME   | When the checksum was computed  |

Invalidate on PUT/DELETE (already tracked in `handleGroveWebDAV`).

Caching may never be needed — grove sync targets `.scion/` directories which
are inherently small.

---

## 4. Client-Side Changes (`pkg/grovesync/grovesync.go`)

Update the rclone on-the-fly remote string to include `vendor=owncloud` and
remove the `ci.CheckSum` workaround:

```go
remote := fmt.Sprintf(":webdav,url='%s',bearer_token='%s',vendor='owncloud':", davURL, opts.AuthToken)
```

Remove:
```go
ci.CheckSum = true // no longer needed — rclone uses OC checksums natively
```

With `vendor=owncloud`, rclone will:
1. Request `oc:checksums` in PROPFIND
2. Parse SHA1/MD5 hashes from the response
3. Use checksums for file comparison automatically

---

## 5. Debug Logging

Add structured debug logging to both sides for troubleshooting sync issues:

### Server (`grove_webdav.go`)

- Log PROPFIND requests with depth and requested properties
- Log checksum computation (path, hash, duration) at DEBUG level
- Log write operations (PUT/DELETE) with file path and size

### Client (`grovesync.go`)

- Log the resolved WebDAV URL and sync direction
- Log rclone's file comparison decisions at DEBUG level
- Pass through rclone's logging when `--debug` is active

---

## 6. Implementation Order

1. **Server: `checksumFile` type** — implement `DeadPropsHolder` with on-the-fly SHA1
2. **Server: wire into `filteredFS.OpenFile()`** — wrap regular files
3. **Client: add `vendor='owncloud'`** to rclone remote string
4. **Client: remove `ci.CheckSum` workaround**
5. **Both: add debug logging**
6. **Tests: unit test `DeadProps()` output format, integration test with rclone**
7. **Future: checksum caching if performance requires it**
