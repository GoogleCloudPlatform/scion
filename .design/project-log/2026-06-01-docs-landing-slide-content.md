# Docs Landing Page: Slide Content Integration

**Date:** 2026-06-01
**PR:** https://github.com/ptone/scion/pull/117
**Branch:** scion/dev-docs-landing

## What Changed

Updated the docs-site landing page (`docs-site/src/pages/landing.astro`) to incorporate content from the Scion explainer slide deck at `https://storage.googleapis.com/scion-intro-slides/index.html`.

### Key Changes

1. **New "Scion Core" pipeline section** — A five-card grid showing the orchestration pipeline: Define → Run → Spawn → Notify → Sync. Each card includes a step number, title, subtitle, and description pulled from the slides.

2. **Expanded feature cards** — Replaced the original six generic feature cards with detailed, slide-sourced descriptions for Agent Definition, Agent Runtime, Agent Collaborators, Agent Notifications, Shared Filesystem, and Harness Agnostic. Each card now includes bullet-pointed sub-features.

3. **Terminology update** — All instances of "Boot" from the slides replaced with "Run". Also updated "grove" to "project" in the quickstart steps.

4. **Slides embed update** — Replaced the old Google Slides embed iframe with the interactive HTML slides deck URL.

## Process Notes

- The slides were fetched via curl since the WebFetch tool had model availability issues.
- Node.js 22+ was required for the Astro build, which wasn't available in the sandbox. Validated the file structure (frontmatter syntax, balanced HTML tags) programmatically instead.
- The landing page is a standalone Astro page (not a Starlight content doc), so it uses raw HTML/CSS rather than MDX components.
