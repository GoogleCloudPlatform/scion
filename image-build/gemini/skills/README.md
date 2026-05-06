# Gemini CLI Custom Skills

This repository contains a collection of custom skills for the [Gemini CLI](https://github.com/google-gemini/gemini-cli). These skills act as "onboarding guides" that extend Gemini CLI's capabilities, providing it with specialized procedural knowledge, workflows, and tools tailored to specific domains and internal project conventions.

## Available Skills

| Skill Package | Description |
|---|---|
| **`go-expert.skill`** | Expert Go developer guidance. Enforces idiomatic Go patterns, robust concurrency, comprehensive testing standards, and specific project conventions for logging and configuration. |
| **`rails-expert.skill`** | Specialized expert for Ruby on Rails development. Provides guidance on idiomatic patterns (Service Objects, Concerns), ActiveRecord performance, Grape API design, testing with RSpec/FactoryBot, and standard background jobs (Sidekiq/Shoryuken). |

*(More skills will be added to this list as the repository grows.)*

## How to Install and Use a Skill

Skills are packaged as `.skill` zip files. To use a skill from this repository in your Gemini CLI sessions, you need to install it.

### 1. Installation

Use the Gemini CLI `skills install` command. You can install a skill either for your current workspace (local) or globally for your user profile.

**To install for the current workspace only:**
```bash
gemini skills install path/to/go-expert.skill --scope workspace
```

**To install globally for all your projects:**
```bash
gemini skills install path/to/go-expert.skill --scope user
```

### 2. Reloading Skills

If you install a skill while you have an active interactive Gemini CLI session running, you must reload your skills within that chat session for it to take effect:
```
/skills reload
```

To verify the skill is installed and active, run:
```
/skills list
```

### 3. Using the Skill

Once installed and active, Gemini CLI automatically decides when to use a skill based on its description and your prompt. You don't usually need to invoke it explicitly by name. Just ask Gemini CLI to perform a task related to the skill's domain.

For example, with the `go-expert` skill installed, you can simply ask:
> "Refactor this Go function to be more idiomatic."
> "Write a table-driven test for this package."
> "Set up the configuration struct using our standard TOML pattern."

Gemini CLI will read your prompt, recognize the overlap with the `go-expert` description, silently activate the skill, and apply its expert guidelines to the generated output.

## Adding New Skills to this Repository

To add a new skill to this repository, it is recommended to use Gemini CLI itself with the built-in `skill-creator` skill.

1. Open Gemini CLI in this repository's root folder.
2. Prompt: `"I want to create a new skill for [Domain/Task]."`
3. Gemini CLI will guide you through understanding the use cases, initializing the folder structure, writing the `SKILL.md` and reference files, and finally packaging it into a `.skill` file.
4. Update this `README.md` with the new skill's name and description.
