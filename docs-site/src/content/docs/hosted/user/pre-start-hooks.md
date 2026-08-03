---
title: Pre-Start Hooks
description: Staging and executing custom shell scripts inside agent containers before startup.
---

Pre-start hooks are in-container shell scripts executed automatically by the Runtime Broker **before** an agent's main harness process launches. They give you a powerful way to customize the agent's filesystem and environment at the very last moment of provisioning — such as installing custom packages, writing specialized configuration files, or verifying external connectivity.

:::note[Pre-Start Hooks vs. Lifecycle Hooks]
Scion features two distinct kinds of hooks that serve different purposes:
1. **Pre-Start Hooks (This guide):** Custom shell scripts running **inside the agent container** on the Runtime Broker *before* the agent process starts. If a pre-start hook fails (exits non-zero), agent startup aborts.
2. **Lifecycle Hooks:** Hub-side webhook or HTTP callbacks running **outside the agent container** on the Hub *after* an agent has successfully crossed a phase transition (e.g., `running`, `stopped`, `suspended`, `error`). For Hub-side transition automation, see the [Lifecycle Hooks Admin Guide](/scion/hosted/ha/lifecycle-hooks/).
:::

---

## Execution Model and Precedence

Every pre-start hook runs inside the agent container under the fixed prefix:

```text
$HOME/.scion/hooks/pre-start.d/30-project-custom
```

Because of this single-slot model, **exactly one pre-start hook can execute per agent container**. When an agent is provisioned, Scion resolves the active hook using a simple fallback/override ordering:

1. **Project-Scoped Hook (Project Settings):** Checked first. If the project contains an active project-scoped hook, it is staged and executed.
2. **Hub-Scoped Hook (Hub Resources):** Checked second as a fallback. If there is no active project-scoped hook, the active hub-scoped default hook (if configured by an administrator) is staged and executed instead.
3. **No Hook:** If neither scope has an active hook, no script is staged, and the container proceeds to start normally.

### Crucial Execution Rules:
- **Failure is Fatal:** If the pre-start hook script exits with a non-zero status, the agent's startup sequence is immediately aborted, and the agent transitions to the `error` phase.
- **Idempotency:** Pre-start hooks execute on *every* container start or resume. Scripts must be idempotent (safe to run multiple times on persistent filesystems).
- **Size Limit:** Hook scripts are stored directly in the Hub database and are capped at **64 KB** at the API layer.

---

## Managing Hooks via CLI

You can manage pre-start hooks using two command trees depending on your permission scope:
- `scion project hook` (for project-scoped hooks; available to project owners)
- `scion hub hook` (for hub-scoped hooks; requires hub-admin privileges)

### Project-Scoped Hooks (`scion project hook`)

Project-scoped hooks let project owners attach customization scripts to a single project. The CLI infers the target project from your current directory's Hub link, or you can specify it explicitly.

```bash
# List all pre-start hooks for the current project
scion project hook list

# Create a new active pre-start hook from a local file
scion project hook create --name "Setup Python Packages" --script setup.sh

# Show the details and content of a specific hook
scion project hook show setup-python-packages

# Update an existing hook script
scion project hook update setup-python-packages --script setup-v2.sh

# Activate an archived hook (automatically archives the currently active hook)
scion project hook activate setup-python-packages

# Delete an archived hook (active hooks cannot be deleted)
scion project hook delete setup-python-packages
```

### Hub-Scoped Default Hooks (`scion hub hook`)

Hub administrators can define a global default pre-start hook that applies to all projects as a baseline fallback.

```bash
# List all hub-scoped pre-start hooks
scion hub hook list

# Create a new active hub default hook from standard input
cat global-bootstrap.sh | scion hub hook create --name "Baseline Tools" --script -

# Show details of a hub-scoped hook
scion hub hook show baseline-tools

# Update an existing hub hook
scion hub hook update baseline-tools --script new-bootstrap.sh

# Activate a hub hook
scion hub hook activate baseline-tools

# Delete an archived hub hook
scion hub hook delete baseline-tools
```

---

## Managing Hooks via the Web UI

Pre-start hooks are also fully supported in the Scion Web Dashboard, matching existing resource-management patterns:

### For Project Owners (Project Settings)
1. Navigate to the project page on the Web UI.
2. Go to the **Project Settings** tab.
3. Under the **Resources** section, select the **Pre-Start Hooks** tab.
4. Here you can view, create, activate, or archive project-scoped hooks. You can also view the inherited Hub-scoped default hook (if any) and see whether it is currently active or overridden.

### For Hub Administrators (Hub Resources)
1. Go to the global **Hub Settings** or **Resources** section (`/settings`).
2. Select the **Pre-Start Hooks** tab.
3. Hub administrators can create, update, activate, and archive hub-scoped fallback scripts here.

---

## Example Customization Scripts

Here are typical use cases for pre-start hooks:

### 1. Installing Custom OS Packages
If your template image is missing utility CLI tools, you can install them on the fly:

```bash
#!/usr/bin/env bash
set -euo pipefail

echo "==> Installing system utilities..."
if ! command -v jq &> /dev/null; then
    sudo apt-get update && sudo apt-get install -y jq
fi
```

### 2. Seeding Configuration Files
Pre-populate configuration files or environment overrides for your agent:

```bash
#!/usr/bin/env bash
set -euo pipefail

echo "==> Setting up local config overrides..."
mkdir -p "$HOME/.config/app"
cat << 'EOF' > "$HOME/.config/app/settings.json"
{
  "api_endpoint": "https://api.internal.yourcompany.com",
  "debug": true
}
EOF
```

### 3. Pre-flight Network Verification
Ensure mandatory internal endpoints are reachable before the agent attempts to run:

```bash
#!/usr/bin/env bash
set -euo pipefail

echo "==> Verifying database connectivity..."
if ! nc -z -w5 db.internal.yourcompany.com 5432; then
    echo "ERROR: Internal database is unreachable. Aborting startup."
    exit 1
fi
```
