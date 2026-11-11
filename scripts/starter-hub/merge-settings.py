#!/usr/bin/env python3
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Merge a generated settings.yaml into the one already on the instance.

gce-start-hub.sh regenerates settings.yaml from a template on every --full
deploy. That template only carries the keys the script manages, so installing
it over an existing file drops everything else -- broker plugin blocks under
server.plugins, server.message_broker, a top-level plugins section -- without
warning.

Usage: merge-settings.py <existing-settings.yaml> <generated-settings.yaml> <output-settings.yaml>

Writes the merged document atomically (mode 0600) to <output-settings.yaml> via
a temporary file in the output directory, leaving both input files untouched.
When <existing-settings.yaml> does not exist or is empty, <generated-settings.yaml>
is written as-is to <output-settings.yaml>.

Merge semantics:
- Generated values win where both files set a key, except for sections listed
  in PRESERVE, which always come from the existing file when present there.
- Mapping merges are additive: keys that an older template wrote and a newer
  template stops emitting are retained from the existing file rather than
  deleted.
- YAML comments are dropped during the safe_load/safe_dump round-trip (mapping
  key order is kept via sort_keys=False).

Exit status is non-zero when either document cannot be parsed as a YAML mapping
or the output file cannot be written; on error <output-settings.yaml> is
neither created nor modified.
"""

import os
import sys
import tempfile

import yaml

# Sections this script does not manage and must never overwrite. Paths are
# relative to the document root. Note: when a path listed here is present in
# the existing file, it silently overrides any value the template may generate
# for that path; when absent from the existing file, any generated value is kept.
PRESERVE = (
    ("server", "plugins"),
    ("server", "message_broker"),
    ("plugins",),
)


def deep_merge(base, override):
    """Layer override on top of base.

    Override wins where both set a value; keys only in base are kept (never
    removed automatically).
    """
    if isinstance(base, dict) and isinstance(override, dict):
        merged = dict(base)
        for key, value in override.items():
            if key in merged:
                merged[key] = deep_merge(merged[key], value)
            else:
                merged[key] = value
        return merged
    return override


def dig(doc, path):
    """Return doc[path[0]][path[1]]..., or None if any step is missing."""
    for key in path:
        if not isinstance(doc, dict) or key not in doc:
            return None
        doc = doc[key]
    return doc


def plant(doc, path, value):
    """Set doc[path[0]][path[1]]..., creating intermediate mappings."""
    for key in path[:-1]:
        child = doc.get(key)
        if not isinstance(child, dict):
            child = {}
            doc[key] = child
        doc = child
    doc[path[-1]] = value


def merge(existing, generated):
    """Return the document to install, plus the preserved section paths."""
    merged = deep_merge(existing, generated)
    kept = []
    for path in PRESERVE:
        value = dig(existing, path)
        if value is not None:
            plant(merged, path, value)
            kept.append(".".join(path))
    return merged, kept


def format_yaml_error(exc):
    """Describe a PyYAML error by line/column only, omitting source snippets."""
    mark = getattr(exc, "problem_mark", None)
    if mark is not None:
        return f"line {mark.line + 1}, column {mark.column + 1}"
    return type(exc).__name__


def atomic_write(output_path, write_fn):
    """Atomically write output_path with mode 0600 via a same-dir temp file."""
    out_dir = os.path.dirname(os.path.abspath(output_path))
    tmp_path = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="w",
            encoding="utf-8",
            dir=out_dir,
            prefix=".settings.yaml.",
            delete=False,
        ) as fh:
            tmp_path = fh.name
            write_fn(fh)
            fh.flush()
            os.fsync(fh.fileno())
        os.chmod(tmp_path, 0o600)
        os.replace(tmp_path, output_path)
        tmp_path = None
    except BaseException:
        if tmp_path is not None:
            try:
                os.unlink(tmp_path)
            except OSError:
                pass
        raise


def main(argv):
    if len(argv) != 4:
        print(
            "usage: merge-settings.py <existing> <generated> <output>",
            file=sys.stderr,
        )
        return 2
    existing_path, generated_path, output_path = argv[1], argv[2], argv[3]

    try:
        with open(generated_path, encoding="utf-8") as fh:
            generated_raw = fh.read()
        generated = yaml.safe_load(generated_raw)
    except OSError as exc:
        print(f"merge-settings: cannot read generated settings: {exc}", file=sys.stderr)
        return 1
    except yaml.YAMLError as exc:
        print(
            f"merge-settings: generated settings is not valid YAML ({format_yaml_error(exc)})",
            file=sys.stderr,
        )
        return 1
    if not isinstance(generated, dict):
        print("merge-settings: generated settings is not a YAML mapping", file=sys.stderr)
        return 1

    try:
        with open(existing_path, encoding="utf-8") as fh:
            existing = yaml.safe_load(fh)
    except FileNotFoundError:
        existing = None
    except OSError as exc:
        print(f"merge-settings: cannot read existing settings: {exc}", file=sys.stderr)
        return 1
    except yaml.YAMLError as exc:
        print(
            f"merge-settings: existing settings is not valid YAML ({format_yaml_error(exc)})",
            file=sys.stderr,
        )
        return 1

    if existing is None:
        try:
            atomic_write(output_path, lambda fh: fh.write(generated_raw))
        except OSError as exc:
            print(f"merge-settings: cannot write merged settings: {exc}", file=sys.stderr)
            return 1
        return 0

    if not isinstance(existing, dict):
        print("merge-settings: existing settings is not a YAML mapping", file=sys.stderr)
        return 1

    merged, kept = merge(existing, generated)
    try:
        atomic_write(
            output_path,
            lambda fh: yaml.safe_dump(
                merged, fh, default_flow_style=False, sort_keys=False
            ),
        )
    except OSError as exc:
        print(f"merge-settings: cannot write merged settings: {exc}", file=sys.stderr)
        return 1
    except yaml.YAMLError as exc:
        print(
            f"merge-settings: cannot serialize merged settings ({format_yaml_error(exc)})",
            file=sys.stderr,
        )
        return 1

    if kept:
        print("merge-settings: preserved " + ", ".join(kept))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
