# syntax=docker/dockerfile:1
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

ARG BASE_IMAGE
FROM ${BASE_IMAGE}

# Exact Copilot CLI release; bump deliberately rather than defaulting to a
# floating tag (passing --build-arg COPILOT_CLI_VERSION=latest still works for
# ad-hoc testing). The build-time smoke check below verifies that the installed
# binary accepts every launch flag configured in config.yaml.
ARG COPILOT_CLI_VERSION=1.0.114

RUN mkdir -p /home/scion/.copilot /home/scion/.copilot/skills \
             /home/scion/.github && \
    chown -R scion:scion /home/scion/.copilot /home/scion/.github

# Pre-seed Copilot settings to prevent interactive prompts (folder trust,
# auto-update banner) that block non-interactive operation. The provisioner
# also writes these defaults, but baking them into the image guarantees
# they exist even if the harness-config home-dir staging doesn't fire.
# settings.json also keeps remote control off ("remoteSessions": false);
# the CLI has no --no-remote flag since 1.0.103.
# config.json is also copilot's sign-in file, so the harness home template
# does not ship it; the provisioner does not treat a config.json holding only
# trustedFolders as a sign-in (see _config_file_may_hold_credential).
COPY --chown=scion:scion home/.copilot/settings.json /home/scion/.copilot/settings.json
RUN printf '{\n  "trustedFolders": ["/workspace"]\n}\n' > /home/scion/.copilot/config.json && \
    chown scion:scion /home/scion/.copilot/config.json

# Install the Copilot CLI from npm (@github/copilot), as the claude harness
# installs Claude Code. npm verifies each tarball against the registry's
# sha512 integrity, and the package pulls the binary for this platform as an
# optional dependency: npm installs only the one matching the build's CPU and
# libc (@github/copilot-linux-x64 or -linux-arm64 on scion-base, which is
# glibc-based Debian trixie). It uses the npm mirror chosen when core-base was
# built (--build-arg NPM_REGISTRY, exported as NPM_CONFIG_REGISTRY, which takes
# precedence over a registry= line in .npmrc). A mirror that needs credentials
# gets them from an .npmrc passed with --secret id=npmrc,src=<path>; the secret
# is never written into a layer. COPILOT_CLI_VERSION=latest resolves to the
# newest published release.
RUN --mount=type=secret,id=npmrc,target=/root/.npmrc,required=false \
    npm cache clean --force \
  && npm install -g "@github/copilot@${COPILOT_CLI_VERSION}" \
  && npm cache clean --force \
  && command -v copilot

# Build-time smoke check: parse the bundle launch flags from config.yaml and
# invoke copilot with --version (which validates all flags before exiting and
# records the installed version in the build log). Fails the image build if a
# CLI release drops or renames a configured flag.
RUN --mount=type=bind,source=config.yaml,target=/tmp/copilot-config.yaml,readonly \
    python3 - /tmp/copilot-config.yaml <<'PY'
import ast, os, re, shlex, subprocess, sys, tempfile

cfg_path = sys.argv[1] if len(sys.argv) > 1 else "/tmp/copilot-config.yaml"
lines = open(cfg_path, encoding="utf-8").read().splitlines()
in_cmd, base, extra = False, [], []
i = 0
while i < len(lines):
    raw = lines[i]
    i += 1
    stripped = raw.strip()
    if not stripped or stripped.startswith("#"):
        continue
    if not raw.startswith(" "):
        in_cmd = stripped == "command:"
        continue
    if not in_cmd:
        continue
    m = re.match(r"^\s+([a-z_]+):\s*(.*)$", raw)
    if not m:
        continue
    key, val = m.group(1), m.group(2).strip()
    if key == "base":
        if val.startswith("["):
            base = ast.literal_eval(val)
        else:
            while i < len(lines) and (not lines[i].strip() or lines[i].strip().startswith("#") or re.match(r"^\s+-\s+", lines[i])):
                bm = re.match(r"^\s+-\s+(.*)$", lines[i])
                if bm:
                    parts = shlex.split(bm.group(1), comments=True)
                    if parts:
                        base.append(parts[0])
                i += 1
    elif key == "resume_flag" and val:
        extra.extend(shlex.split(val, comments=True))
    elif key == "task_flag" and val:
        extra.extend(shlex.split(val, comments=True))
        extra.append("smoke-check")

if not base:
    sys.exit("smoke-check: failed to parse command.base from config.yaml")
cmd = list(base) + extra + ["--version"]
print("smoke-check:", " ".join(shlex.quote(c) for c in cmd))
with tempfile.TemporaryDirectory() as tmp_home:
    env = dict(os.environ, HOME=tmp_home)
    subprocess.run(cmd, check=True, env=env)
PY

CMD ["copilot"]
