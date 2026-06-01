# Docs Landing Page: Inline Slideshow Integration

**Date:** 2026-06-01
**PR:** https://github.com/ptone/scion/pull/117
**Branch:** scion/dev-docs-landing

## What Changed

Integrated the full Scion explainer slide deck directly into the docs-site landing page (`docs-site/src/pages/landing.astro`) as an interactive inline slideshow component, replacing the previous iframe embed approach.

### Key Changes

1. **Inline slideshow component** — All 7 slides from the hosted deck at `https://storage.googleapis.com/scion-intro-slides/index.html` are now rendered directly in the page. Includes keyboard/touch/click navigation, progress dots, and smooth CSS transitions. The slideshow is contained in a 16:9 aspect-ratio container scoped under `.scion-deck`.

2. **Interactive widgets recreated inline** — The State Model Simulator, Collaborators Graph (with dynamic agent spawn/delete loop), Reactive Notification Wakeup flow, and Shared Filesystem Simulator are all fully functional with inline JS.

3. **Scoped CSS** — All slideshow styles are namespaced under `.scion-deck` to avoid conflicts with the landing page's own design system (which uses different CSS variables).

4. **Expanded feature cards** — Below the slideshow, detailed feature cards provide the text content from each slide topic (Agent Definition, Runtime, Collaborators, Notifications, Shared Filesystem, Harness Agnostic).

5. **Terminology update** — All instances of "Boot" replaced with "Run". Also updated "grove" to "project" in the quickstart steps.

6. **Section reordering** — Removed the Overview Deck iframe section. Added a "technical deep dive" link to the Google Slides deck. Moved the quickstart section below the video section.

## Process Notes

- The slides HTML/CSS/JS were fetched via curl (WebFetch tool had model availability issues) and carefully adapted for inline embedding.
- Node.js 22+ was required for the Astro build, which wasn't available in the sandbox. Validated file structure (balanced HTML tags, frontmatter syntax) programmatically.
- The landing page is a standalone Astro page (not a Starlight content doc), so it uses raw HTML/CSS and an `is:inline` script tag.
- The slideshow CSS was significantly adapted from the original (viewport-based sizing → container-based, proportionally scaled typography).
