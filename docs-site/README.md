# Scion Documentation Site

This is the documentation site for [Scion](https://github.com/GoogleCloudPlatform/scion), built using [Starlight](https://starlight.astro.build/).

## 🚀 Project Structure

```
.
├── public/          # Static assets (favicons, etc.)
├── src/
│   ├── assets/      # Images and other media
│   ├── content/
│   │   └── docs/    # Markdown/MDX documentation files
│   └── content.config.ts
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

| Command                   | Action                                           |
| :------------------------ | :----------------------------------------------- |
| `npm install`             | Installs dependencies                            |
| `npm run dev`             | Starts local dev server at `localhost:4321`      |
| `npm run build`           | Build the production site to `./dist/`           |
| `npm run preview`         | Preview the build locally                        |
| `npm run astro ...`       | Run CLI commands like `astro add`, `astro check` |

## 🚢 Deployment

### GitHub Pages

The docs site is automatically built and deployed to GitHub Pages on pushes to `main` that change files in `docs-site/`. The workflow can also be triggered manually from the Actions tab.

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