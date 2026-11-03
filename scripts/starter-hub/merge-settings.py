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

Usage: merge-settings.py <existing-settings.yaml> <generated-settings.yaml>

The generated file is rewritten in place with the merged document, which the
caller then installs. Generated values win, except for the sections in
PRESERVE, which always come from the existing file.

Exit status is non-zero when either document cannot be parsed, so the caller
can fall back to its previous behaviour.
"""

import sys

import yaml

# Sections this script does not manage and must never overwrite. Paths are
# relative to the document root.
PRESERVE = (
    ("server", "plugins"),
    ("server", "message_broker"),
    ("plugins",),
)


def deep_merge(base, override):
    """Layer override on top of base. Override wins where both set a value."""
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


def main(argv):
    if len(argv) != 3:
        print("usage: merge-settings.py <existing> <generated>", file=sys.stderr)
        return 2
    existing_path, generated_path = argv[1], argv[2]

    try:
        with open(generated_path, encoding="utf-8") as fh:
            generated = yaml.safe_load(fh)
    except (OSError, yaml.YAMLError) as exc:
        print(f"merge-settings: cannot read generated settings: {exc}", file=sys.stderr)
        return 1
    if not isinstance(generated, dict):
        print("merge-settings: generated settings is not a YAML mapping", file=sys.stderr)
        return 1

    try:
        with open(existing_path, encoding="utf-8") as fh:
            existing = yaml.safe_load(fh)
    except FileNotFoundError:
        # Nothing to merge into; the caller installs the generated file as-is.
        return 0
    except OSError as exc:
        print(f"merge-settings: cannot read existing settings: {exc}", file=sys.stderr)
        return 1
    except yaml.YAMLError as exc:
        print(f"merge-settings: existing settings is not valid YAML: {exc}", file=sys.stderr)
        return 1

    if existing is None:
        return 0
    if not isinstance(existing, dict):
        print("merge-settings: existing settings is not a YAML mapping", file=sys.stderr)
        return 1

    merged, kept = merge(existing, generated)
    try:
        with open(generated_path, "w", encoding="utf-8") as fh:
            yaml.safe_dump(merged, fh, default_flow_style=False, sort_keys=False)
    except OSError as exc:
        print(f"merge-settings: cannot write merged settings: {exc}", file=sys.stderr)
        return 1

    if kept:
        print("merge-settings: preserved " + ", ".join(kept))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
