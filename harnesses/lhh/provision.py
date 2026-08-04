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
"""LHH (Long-Horizon Harness) container-side provisioner.

Runs inside the agent container during the pre-start lifecycle hook, invoked
by `sciontool harness provision --manifest ...`. The host-side
ContainerScriptHarness has already:

  * Staged this script and config.yaml under $HOME/.scion/harness/.
  * Written inputs/auth-candidates.json with the env-var names + paths to
    secret-value files under $HOME/.scion/harness/secrets/<NAME>.
  * Mounted ADC credentials when vertex-ai mode is in use.

This script's job:

  1. Determine which auth method LHH will use — vertex-ai only (LHH requires
     Vertex AI, no API key mode).
  2. Resolve SCION_MODEL alias to a concrete Gemini model name for
     LHA_ROOT_MODEL.
  3. Project the system prompt + agent instructions into ~/.lhh/AGENTS.md
     (with skills inlined, since LHH has no native Scion skills directory).
  4. Write outputs/resolved-auth.json and outputs/env.json.
"""

from __future__ import annotations

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import scion_harness  # type: ignore[import-not-found]

assert scion_harness.INTERFACE_VERSION >= 2, (
    "lhh provision.py requires scion_harness INTERFACE_VERSION >= 2; "
    f"got {scion_harness.INTERFACE_VERSION}"
)

AUTH = scion_harness.AuthSpec(
    "lhh",
    [
        scion_harness.env_method(
            "vertex-ai",
            any_of=["GOOGLE_CLOUD_PROJECT"],
            hint="set GOOGLE_CLOUD_PROJECT (with ADC or GCP service account) for Vertex AI",
        ),
    ],
)


def _resolve_model(ctx: scion_harness.ProvisionContext) -> str:
    """Resolve SCION_MODEL alias to a concrete Gemini model name."""
    raw = os.environ.get("SCION_MODEL", "").strip()
    if not raw:
        return "gemini-3.6-flash"  # LHH default

    aliases: dict[str, str] = ctx.harness_config.get("model_aliases") or {}
    normalized = raw.lower()
    shorthand = {"s": "small", "m": "medium", "l": "large", "xl": "extra-large"}
    normalized = shorthand.get(normalized, normalized)

    concrete = aliases.get(normalized)
    if concrete:
        ctx.info(f"resolved model alias {raw!r} → {concrete!r}")
        return concrete
    return raw  # already a concrete model name


def _build_env_overlay(model: str) -> dict[str, str]:
    """Build the env vars to project into the harness process."""
    return {
        "GOOGLE_CLOUD_PROJECT": "${GOOGLE_CLOUD_PROJECT}",
        "GOOGLE_CLOUD_REGION": "${GOOGLE_CLOUD_REGION}",
        "GOOGLE_CLOUD_LOCATION": "${GOOGLE_CLOUD_REGION}",
        "GOOGLE_GENAI_USE_VERTEXAI": "true",
        "LHA_ROOT_MODEL": model,
        "LHA_ENVIRONMENT_BACKEND": "local",
    }


def provision(ctx: scion_harness.ProvisionContext) -> None:
    resolved = ctx.select_auth(AUTH)

    model = _resolve_model(ctx)
    env = _build_env_overlay(model)

    # Project instructions (system prompt prepended into AGENTS.md).
    # include_skills=True because LHH has no native Scion skills directory —
    # skills are inlined into the instructions file instead.
    harness_cfg = ctx.harness_config
    instructions_file = str(harness_cfg.get("instructions_file") or ".lhh/AGENTS.md")
    scion_harness.project_instructions(ctx, instructions_file, include_skills=True)

    # Write LHH config directory
    lhh_config_dir = os.path.join(ctx.home, ".lhh")
    os.makedirs(lhh_config_dir, exist_ok=True)

    ctx.write_outputs(resolved, env=env, extra={"vertex_ai": True})
    ctx.info(f"method={resolved.method} model={model}")


if __name__ == "__main__":
    scion_harness.run("lhh", provision)
