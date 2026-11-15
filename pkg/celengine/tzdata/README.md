# Embedded IANA Time Zone Archive (`zoneinfo.zip`)

`zoneinfo.zip` is embedded into `pkg/celengine` via `//go:embed` (`pkg/celengine/tzdata.go`) and resolved exclusively through `time.LoadLocationFromTZData`.

`pkg/celengine` deliberately does **not** use `time.LoadLocation` or `_ "time/tzdata"`, because `time.LoadLocation` consults `$ZONEINFO`, `/usr/share/zoneinfo`, and `$GOROOT/lib/time/zoneinfo.zip` on the host filesystem before falling back to embedded `time/tzdata`. Resolving from this archive guarantees deterministic, zero-disk IANA time zone evaluation across replicas and containers.

## Provenance and License

- **Source Go toolchain:** `go1.26.1` (`$(go env GOROOT)/lib/time/zoneinfo.zip`)
- **IANA Time Zone Database version:** `tzcode2025c` / `tzdata2025c` (per `$(go env GOROOT)/lib/time/update.bash`)
- **License:** Public domain. Per IANA (`https://www.iana.org/time-zones`, RFC 6557) and `$(go env GOROOT)/lib/time/README`:
  > The `zoneinfo.zip` archive contains time zone files compiled using the code and data maintained as part of the IANA Time Zone Database. The IANA asserts that the database is in the public domain.

## Drift Detection and Regeneration

`TestEmbeddedTZDataMatchesGOROOTZoneinfo` (`pkg/celengine/engine_test.go`) compares every entry in `pkg/celengine/tzdata/zoneinfo.zip` byte-for-byte against `$(go env GOROOT)/lib/time/zoneinfo.zip` whenever that file is present, catching drift automatically on Go toolchain upgrades.

To regenerate `pkg/celengine/tzdata/zoneinfo.zip` deterministically from the active Go toolchain:

```bash
python3 - <<'PY'
import os, subprocess, zipfile

goroot = subprocess.check_output(["go", "env", "GOROOT"], text=True).strip()
src = os.path.join(goroot, "lib", "time", "zoneinfo.zip")
dst = "pkg/celengine/tzdata/zoneinfo.zip"

with zipfile.ZipFile(src, "r") as zin:
    entries = sorted(
        (info.filename, zin.read(info.filename))
        for info in zin.infolist()
        if not info.is_dir()
    )

with zipfile.ZipFile(dst, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as zout:
    for name, data in entries:
        zi = zipfile.ZipInfo(filename=name, date_time=(1980, 1, 1, 0, 0, 0))
        zi.compress_type = zipfile.ZIP_DEFLATED
        zi.external_attr = 0o644 << 16
        zout.writestr(zi, data)
PY
```
