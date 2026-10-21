---
name: scion-web-serving
description: >-
  How to serve a web app or page from inside an agent so a human can open it
  through scion expose: ship one self-contained HTML file, the build settings
  each common tool needs for that, how to hand over the right URL, and how to
  verify it honestly.
---

# Serving web apps through scion expose

A person opens your page in their own browser through a link on the Hub. Today
such a page can only use what is inside the page itself: separate scripts,
stylesheets, images and requests back to your app are not served. Build for
that, or point the person to `scion tunnel` (below).

## Steps

1. Build the page as **one self-contained HTML file** (see Build settings).
2. Serve it on `127.0.0.1` on a port of 1024 or above, not 9810 or 18380, as
   `index.html` so the root URL opens it:
   `python3 -m http.server <port> --bind 127.0.0.1 --directory <folder>`
3. Run: `scion expose <port> --label "<what it is>"`
4. Verify it (see Verifying honestly).
5. Give the human the URL **exactly as printed**, including the trailing `/`.
   Do not hand out tokens, and do not build the URL by hand.

## What works when a person opens the link

- Works: one HTML file with its CSS in `<style>`, its JavaScript in `<script>`
  (no `src`), and images and fonts as `data:` URLs.
- Does not work: `<script src>`, `<link rel="stylesheet" href>`, `<img src>`
  pointing at your server, `fetch`, XHR, EventSource or WebSocket calls back
  to your app. This holds for relative URLs (`./app.js`) and for URLs that
  start with `/` alike.
- Does not work: localStorage, sessionStorage, IndexedDB, cookies, web workers
  and service workers.
- Does not work: dev servers with live reload (`vite dev`, `next dev`), and
  anything that needs a live backend.
- Data the page needs must be inlined at build time (for example a
  `<script type="application/json">` block or a JS constant).

For a full multi-file app, a backend API or a dev server, the human can use
`scion tunnel` from their machine with the scion CLI, if their Hub has
`scion tunnel` turned on. Suggest:

    scion tunnel <agent> --port <port>

It opens the app on a local port with everything working. See
https://googlecloudplatform.github.io/scion/hosted/user/port-forwarding/#multi-file-apps

## Build settings (single-file output)

- **Vite** (React, Vue, Svelte, plain): add `vite-plugin-singlefile`
  (`npm i -D vite-plugin-singlefile`, then `plugins: [..., viteSingleFile()]`
  in `vite.config.*`) and run `vite build`. `dist/index.html` is the whole
  app. Serve `dist`. Check that nothing was left as a separate file that the
  page still loads (large images or fonts may need inlining by hand).
- **Plain HTML**: put the CSS in `<style>` and the JS in `<script>`. Turn
  images into data URLs, for example
  `echo "data:image/png;base64,$(base64 -w0 logo.png)"`.
- **Other bundlers** (webpack, Create React App, Parcel, esbuild): use the
  bundler's inline option or plugin so the output is one HTML file with no
  separate assets; if that is not practical, rebuild the page with Vite and
  `vite-plugin-singlefile`.
- **Next.js, server-rendered apps, apps with an API**: not supported through
  the link. Use `scion tunnel`, or produce a static single-file page with the
  data inlined.
- **Static servers**: `python3 -m http.server <port> --bind 127.0.0.1
  --directory <folder>` serves the folder as is. Any static server bound to
  `127.0.0.1` works; what matters is the single file it serves.

## Verifying honestly

- A `curl` with your own agent token is NOT proof. It only shows that the
  file is served; it does not behave like a person's browser and will succeed
  for pages that are broken for them.
- Search the built HTML for `src=`, `href=`, `url(`, `fetch(`, `XMLHttpRequest`,
  `EventSource`, `WebSocket`, `import(` and `localStorage`. Anything that
  points at your server or at browser storage will not work through the link.
- Load the page in a headless browser (for example Playwright Chromium) from
  `http://127.0.0.1:<port>/`, with every request other than the page itself
  blocked (in Playwright: `page.route('**/*', r => r.request().isNavigationRequest() ? r.continue() : r.abort())`).
  Check that the page renders what it should, that the console has no
  errors, and that no request was blocked.
- Report what you verified and how (which checks, which browser). If you
  could not run a browser, say so plainly instead of reporting the page as
  working.
