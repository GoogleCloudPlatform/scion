# Implementation Plan - Harness Skills Support

## 1. Executive Summary
This plan outlines the implementation of a standardized "Skills" support for Scion agents. A new standard for LLM agents involves an "Agent Skills" directory (containing `SKILL.md` and related scripts) that enables agents to perform specialized tasks. We will enable Scion templates to provide these skills by automatically discovering a `skills/` directory in a template and mounting it into the correct harness-specific location within the agent container.

## 2. Analysis & Findings
*   **Context:** Modern LLM agents (Claude Code, Gemini CLI, etc.) support a "skills" directory. These skills are discovered at runtime and provide additional capabilities to the agent.
*   **Harness Specifics:**
    *   **Claude Code:** Discovers skills in `~/.claude/skills/`.
    *   **Gemini CLI:** Discovers skills in `~/.gemini/skills/`.
    *   **OpenCode:** Follows a pattern consistent with `~/.config/opencode/skills/`.
    *   **Codex:** Follows a pattern consistent with `~/.codex/skills/`.
*   **Architecture:** Scion uses an isolation model where agent-specific configuration and state are stored in a dedicated `home` directory on the host (`agentHome`), which is then mounted as the user's home directory in the container (e.g., `/home/scion`).
*   **Constraints:** Currently, Scion templates only copy the `home/` subdirectory during provisioning. Top-level `skills/` directories in templates are ignored.

## 3. Implementation Plan

### Phase 1: Interface & Harness Updates
*   [x] **Action:** Add `SkillsDir()` to the `api.Harness` interface.
    *   **Files:** `pkg/api/harness.go`
    *   **Details:** Add `SkillsDir() string` to the interface to allow each harness to define its skill mount point.
*   [x] **Action:** Implement `SkillsDir()` for each harness.
    *   **Files:** 
        *   `pkg/harness/gemini_cli.go`: Return `.gemini/skills`
        *   `pkg/harness/claude_code.go`: Return `.claude/skills`
        *   `pkg/harness/opencode.go`: Return `.config/opencode/skills`
        *   `pkg/harness/codex.go`: Return `.codex/skills` (and update `DefaultConfigDir()` to return `.codex` instead of empty string for consistency)
        *   `pkg/harness/generic.go`: Return `.scion/skills` (consistent with its `DefaultConfigDir()`)

### Phase 2: Provisioning Logic
*   [x] **Action:** Update `ProvisionAgent` to handle the `skills/` directory.
    *   **Files:** `pkg/agent/provision.go`
    *   **Details:** 
        1.  In `ProvisionAgent`, after copying `home/` files, loop through the template chain.
        2.  For each template, check if a `skills/` directory exists at the root.
        3.  If it exists, copy its contents to `agentHome/<harness.SkillsDir()>` using `util.CopyDir` to allow overlay/merge behavior.
        4.  Also apply this to the `skills/` directory (if any) in the `harness-config` base layer.

### Phase 3: Templates & Examples
*   [x] **Action:** Add a placeholder `skills/` directory to the default template.
    *   **Files:** `pkg/config/embeds/templates/default/skills/.gitkeep`
    *   **Details:** Ensure that `scion init` or agent creation creates a visible structure for users to follow.
*   [x] **Action:** (Optional) Add a built-in "scion" skill.
    *   **Details:** Done — skills are now embedded as platform skills in `resources/platform_skills/` and injected via `injectPlatformSkills()`. The root `skills/` directory and workspace skill overlay have been removed.

## 4. Verification Strategy
*   **Automated Tests:**
    *   Add a new test in `pkg/agent/provision_test.go` that:
        1.  Creates a mock template with a `skills/` directory.
        2.  Provisions an agent using that template.
        3.  Verifies that the files are correctly placed in `agentHome/.<harness>/skills/`.
*   **Manual Verification:**
    1.  Create a custom template with a `skills/my-skill/SKILL.md`.
    2.  Provision an agent with `scion start --template my-template`.
    3.  Attach to the agent and verify the skill is available (e.g., using slash commands or checking the file system).

## 5. Risks & Unknowns
*   **Overlay Conflicts:** If multiple templates in a chain provide the same skill directory name, files will be merged. This is generally desired but may cause confusion if not documented.
*   **Harness Support:** While Claude and Gemini are known to support this standard, other harnesses may ignore the directory until their images are updated. However, Scion's responsibility is to ensure the files are correctly placed.
