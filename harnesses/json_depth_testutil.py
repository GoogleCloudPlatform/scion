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
"""Test helper: a deterministic JSON decoder nesting limit.

Since Python 3.14 the C JSON decoder's nesting limit follows the stack size:
about 65000 levels with an 8 MiB stack, and no limit with an unlimited one.
Whether content is "too deep for Python" now depends on the machine
(ptone/scion#6254), so tests of that path simulate a decoder limit instead of
relying on the interpreter's.

Standalone on purpose (imports nothing from scion_harness): harness test
suites that vendor their own scion_harness load it by path.
"""

from __future__ import annotations

import json
from contextlib import contextmanager
from typing import Iterator
from unittest import mock

# Well below the limit any supported CPython decoder enforces, so content
# within it still decodes for real.
SIMULATED_DECODER_DEPTH_LIMIT = 500

_RECURSION_MESSAGE = "maximum recursion depth exceeded while decoding a JSON document"


def first_too_deep_offset(text: str, limit: int) -> int | None:
    """Offset of the first bracket nesting text deeper than limit (brackets
    inside strings don't count), or None if it never does."""
    depth = 0
    in_string = escaped = False
    for i, ch in enumerate(text):
        if in_string:
            if escaped:
                escaped = False
            elif ch == "\\":
                escaped = True
            elif ch == '"':
                in_string = False
        elif ch == '"':
            in_string = True
        elif ch in "[{":
            depth += 1
            if depth > limit:
                return i
        elif ch in "]}":
            depth -= 1
    return None


@contextmanager
def simulated_decoder_depth_limit(limit: int = SIMULATED_DECODER_DEPTH_LIMIT) -> Iterator[None]:
    """Make json.loads (and json.load) stop at limit levels of nesting the
    way a decoder at its limit does.

    The document is decoded for real up to the first bracket past the limit,
    so errors before it are raised as usual. Reaching that bracket where a
    value is expected raises RecursionError; a bracket where no value can go
    raises the decoder's JSONDecodeError, as the real decoder does.
    """
    real_loads = json.loads

    def loads(s, *args, **kwargs):
        text = s
        if isinstance(s, (bytes, bytearray)):
            # As json.loads decodes bytes.
            text = s.decode(json.detect_encoding(s), "surrogatepass")
        cut = first_too_deep_offset(text, limit)
        if cut is None:
            return real_loads(s, *args, **kwargs)
        try:
            real_loads(text[:cut], *args, **kwargs)
        except json.JSONDecodeError as exc:
            # A prefix cut at a value position fails only with "Expecting
            # value" at the cut; any other error is real.
            if exc.pos < cut or exc.msg != "Expecting value":
                raise
        raise RecursionError(_RECURSION_MESSAGE)

    with mock.patch.object(json, "loads", loads):
        yield
