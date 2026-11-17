# Scion Documentation Site

This is the documentation site for [Scion](https://github.com/GoogleCloudPlatform/scion), built using [Starlight](https://starlight.astro.build/).

## 🚀 Project Structure

```
.
├── public/          # Static assets (favicons, fallback versions.json, etc.)
├── scripts/         # Snapshot link verification, legacy backfill, and Pages assembly scripts
├── src/
│   ├── assets/      # Images and other media
│   ├── components/  # Starlight component overrides (VersionSelect, ThemeSelect, Banner)
│   ├── content/
│   │   └── docs/    # Markdown/MDX documentation files
│   ├── lib/         # Version classification, URL rewriting, and client manifest loader
│   ├── plugins/     # Astro/Sätteri `/scion/...` base-path link rewriter
│   └── content.config.ts
├── test/            # Node built-in (`node --test`) unit tests
├── astro.config.mjs # Astro configuration
├── package.json
└── tsconfig.json
```

## 🛠 Prerequisites

This site uses **D2** for diagrams via the `astro-d2` integration. To build the site locally with diagrams, you must have the `d2` CLI installed.

### Installing D2

```bash
curl -fsSL https://d2lang.com/install.sh | sh -s --
```

## 🧞 Commands

All commands are run from the `docs-site` directory:

| Command                   | Action                                                    |
| :------------------------ | :-------------------------------------------------------- |
| `npm ci`                  | Installs locked dependencies                              |
| `npm run dev`             | Starts local dev server at `localhost:4321/scion/`        |
| `npm test`                | Runs unit tests (`node --test test/*.test.mjs`)           |
| `npm run check`           | Runs `astro check` type and template diagnostics          |
| `npm run check-d2`        | Validates all embedded D2 diagram blocks                  |
| `npm run build`           | Builds the production site (`main` default) to `./dist/`  |
| `npm run verify-snapshot` | Verifies a snapshot `./dist/` has zero unprefixed `/scion/` links |
| `npm run preview`         | Previews the build locally                                |
| `npm run astro ...`       | Runs CLI commands like `astro add`, `astro check`         |

### Building a Versioned Snapshot Locally

By default `npm run build` builds the `main` channel at `base: '/scion'`. To build a release snapshot locally under `/scion/<tag>/`:

```bash
DOCS_BASE=/scion/v0.3.0 DOCS_CHANNEL=stable DOCS_VERSION=v0.3.0 npm run build
DOCS_BASE=/scion/v0.3.0 npm run verify-snapshot
```

Or build a complete release tarball (`scion-docs-<tag>.tar.gz` + `.sha256`) from any git tag—including historical tags prior to native versioning—from the repository root:

```bash
./scripts/docs/build-release-snapshot.sh --tag v0.3.0 --out-dir /tmp/docs-snapshots
```

## 🚢 Deployment

### GitHub Pages (Multi-Version)

The documentation site is published to GitHub Pages (`https://googlecloudplatform.github.io/scion/`) as an assembled multi-version site:

- `/scion/<slug>/` serves the latest docs from `main`.
- `/scion/<tag>/<slug>/` serves immutable tag snapshots (`Stable`, the single most recent active `Preview`/`RC` build overall across all unreleased lines, and `Nightly` builds from the last 14 days).
- `/scion/main/`, `/scion/stable/`, `/scion/preview/`, and `/scion/nightly/` redirect to the latest `/overview/` page in each channel.
- `/scion/versions.json` lists available channels and versions for the header channel/version selectors and outdated-snapshot banner.

Release workflows call `.github/workflows/docs-snapshot.yml` to build and upload `scion-docs-<tag>.tar.gz` (plus `.sha256`) to each GitHub Release. `.github/workflows/docs.yml` runs on `main` pushes, manual dispatch, and `workflow_run` completions to assemble `main` + retained release tarballs via `scripts/assemble-pages.mjs` and deploy to GitHub Pages from `refs/heads/main`. See [`docs/release-process.md`](../docs/release-process.md#versioned-documentation-snapshots) for operational details and backfill commands.

**Repository setup:** In the GitHub repo settings, go to **Settings → Pages → Source** and set it to **GitHub Actions**.

### Google Cloud Run

The documentation site is also containerized and can be deployed to Google Cloud Run.

### Local Container Build

To build and run the container locally:

```bash
# From the docs-site directory
docker build -t scion-docs .
docker run -p 8080:8080 scion-docs
```

### Cloud Build & Cloud Run

To build and deploy using Google Cloud Build:

```bash
# From the docs-site directory
gcloud builds submit --config cloudbuild.yaml .
# Or use the provided script
./deploy.sh
```

The Cloud Build configuration:
- Builds the image with `docs-site/Dockerfile`.
- Pushes the image to Artifact Registry.
- Deploys the image to a Cloud Run service named `scion-docs`.

## 📝 Writing Documentation

Documentation is written in Markdown (`.md`) or MDX (`.mdx`) in the `src/content/docs/` directory.

### Diagrams

You can include D2 diagrams directly in your documentation using `d2` code blocks:

```d2
User -> Scion: Start Agent
Scion -> Container: Run
```

The diagrams will be automatically rendered as SVGs during the build process.