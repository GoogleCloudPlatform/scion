---
title: GCP Skill Registry
description: Import and manage Scion skills in the GCP Vertex AI Skill Registry using the Agent Platform API.
---

The GCP Skill Registry lets you store and serve skills through Google Cloud's Vertex AI Agent Platform. Once imported, skills can be referenced from any `scion-agent.yaml` using the `gcp-skill://` URI scheme — no Scion Hub required.

This tutorial walks through setting up the registry, importing skills from a GitHub repository, and verifying the results using the helper scripts in `scripts/skill-bank/gcp/`.

## Prerequisites

- A GCP project with the **Agent Platform API** (`aiplatform.googleapis.com`) enabled
- Python 3.8+
- [Application Default Credentials](https://cloud.google.com/docs/authentication/provide-credentials-adc) configured
- The scripts from `scripts/skill-bank/gcp/` in your local checkout

### Enable the API

```bash
gcloud services enable aiplatform.googleapis.com --project=YOUR_PROJECT_ID
```

### Authenticate

```bash
gcloud auth application-default login
```

Or, for a service account:

```bash
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/key.json
```

Your account needs the `roles/aiplatform.user` IAM role on the project.

## Install dependencies

The scripts require a few Python packages:

```bash
pip install -r scripts/skill-bank/gcp/requirements.txt
```

This installs:

- `google-auth` — GCP authentication
- `google-auth-httplib2` — HTTP transport for auth
- `requests` — HTTP client
- `pyyaml` — YAML parsing for `SKILL.md` frontmatter

## Configure the registry

Edit `scripts/skill-bank/gcp/config.py` to point at your project and region:

```python
PROJECT_ID = "your-gcp-project-id"
LOCATION = "us-central1"
API_BASE = f"https://{LOCATION}-aiplatform.googleapis.com/v1beta1"
SKILLS_PARENT = f"projects/{PROJECT_ID}/locations/{LOCATION}"
SKILLS_URL = f"{API_BASE}/{SKILLS_PARENT}/skills"
```

Change `PROJECT_ID` to your GCP project and `LOCATION` to your preferred region (e.g. `us-central1`, `europe-west1`).

## Verify registry access

The Skill Registry is a project-level service — there is no separate creation step. Run `create_registry.py` to confirm the API is enabled and accessible:

```bash
python3 scripts/skill-bank/gcp/create_registry.py
```

Expected output:

```text
Project:  your-gcp-project-id
Location: us-central1
Endpoint: https://us-central1-aiplatform.googleapis.com/v1beta1/projects/your-gcp-project-id/locations/us-central1/skills

Registry is accessible. 0 skill(s) found.
```

If you see a 403 or 404 error, double-check that the API is enabled and your credentials have the `roles/aiplatform.user` role.

## Import skills from a GitHub repository

The `import_skills.py` script clones a skill repository, discovers all valid skill directories, and uploads each one to the registry:

```bash
python3 scripts/skill-bank/gcp/import_skills.py
```

Example output:

```text
Cloning https://github.com/mattpocock/skills.git...

Found 24 skills to import.

  Importing engineering/api-design... OK
  Importing engineering/code-review... OK
  Importing engineering/debugging... OK
  Importing misc/markdown-formatting... OK
  Importing productivity/time-management... ALREADY EXISTS (skipped)
  ...

Results: 22 imported, 2 already existed, 0 failed
Total skills in repo: 24
```

### How skill discovery works

The script walks the cloned repository looking for directories that contain a `SKILL.md` file. Each `SKILL.md` must have YAML frontmatter with `name` and `description` fields:

```markdown
---
name: code-review
description: Guidelines for thorough code reviews.
---

# Code Review

Review checklist and best practices...
```

The entire skill directory (including any supporting files) is zipped and uploaded to the registry. Skills that already exist in the registry are skipped (HTTP 409).

:::note
The default script imports from [mattpocock/skills](https://github.com/mattpocock/skills). To import from a different repository, edit the `SKILLS_REPO` variable in `import_skills.py`.
:::

## Verify the import

List all skills in the registry to confirm the import succeeded:

```bash
python3 scripts/skill-bank/gcp/list_skills.py
```

Example output:

```text
Listing skills in your-gcp-project-id / us-central1

Name                                               Display Name                   State
------------------------------------------------------------------------------------------
api-design                                         api-design                     ACTIVE
code-review                                        code-review                    ACTIVE
debugging                                          debugging                      ACTIVE
markdown-formatting                                markdown-formatting            ACTIVE
time-management                                    time-management                ACTIVE

Total: 5 skill(s)
```

## Using imported skills in templates

Once skills are in the GCP registry, reference them from a `scion-agent.yaml` using the `gcp-skill://` URI scheme:

```yaml
# scion-agent.yaml
schema_version: "1"
name: my-agent

skills:
  - uri: gcp-skill://my-registry/code-review
  - uri: gcp-skill://my-registry/api-design@1.0.0
    optional: true
```

The `gcp-skill://` scheme follows the format `gcp-skill://<alias>/<skillId>@<version>`, where:

- **`alias`** — the registry alias configured in your [skill federation settings](/hosted/single-node/skill-registry/)
- **`skillId`** — the skill name as registered in GCP
- **`version`** — an optional version specifier (defaults to latest)

For the federation setup that connects your Scion Hub to a GCP registry, see [Skill Registry & Federation](/hosted/single-node/skill-registry/).

## See also

- [Skills — Authoring & Publishing](/local/skills/) — the full skills guide, including URI formats and scopes.
- [Skill Registry & Federation](/hosted/single-node/skill-registry/) — Hub-side registry administration and external registry federation.
