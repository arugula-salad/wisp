# Project website

The GitHub Pages site lives in `site/`. It is plain HTML, CSS, and JavaScript with no
build step or runtime dependencies. Only that directory is published; the dashboard
and its API stay on your host.

## Preview

From the repository root:

```sh
python3 -m http.server 7790 --directory site
```

Open <http://localhost:7790>. All asset paths are relative, so the same files work at
`https://arugula-salad.github.io/wisp/` or under a custom domain.

## Publish

In the repository's **Settings → Pages → Build and deployment**, select **GitHub
Actions** as the source. The `Deploy GitHub Pages` workflow publishes `site/` when
changes to it or the workflow land on `main`. You can also run it manually from
Actions. The deployment URL appears on the workflow's `github-pages` environment.

The workflow follows GitHub's [custom Pages workflow instructions](https://docs.github.com/en/pages/getting-started-with-github-pages/using-custom-workflows-with-github-pages).
It grants Pages and OIDC write permissions only to the deployment job and queues
concurrent deployments rather than cancelling a deployment in progress.

## Screenshots

The 15 images in `site/assets/screenshots/` were captured from a running wisp dashboard
at `http://localhost:7788/ui/` on September 23, 2026, in dark mode at a 1440 × 1000
viewport. Captures include the full page, so some images are taller than the viewport.
They show actual host activity, not seeded or fabricated metrics. The terminal uses
the `hello` sprite and read-only commands (`uname`, `whoami`, `pwd`, and `df`).

To refresh a screenshot:

1. Sign into your local dashboard using the host token. Never include the login token,
   authentication cookies, or browser storage in the published files.
2. Use a 1440 × 1000 viewport, dark theme, and the corresponding page/tab. Wait for the
   charts and data to load. Avoid modifying policies, restoring checkpoints, or
   stopping workloads just for a screenshot.
3. Capture the full page and inspect it for private file contents, credentials, or
   other data you do not want published. A terminal session wakes the selected sprite;
   disconnect it when finished.
4. Save as lossless WebP under the existing filename. Update the image's intrinsic
   `width` and `height` attributes in `site/index.html` if its dimensions change.

The gallery crops previews from the top; click an image for its full-page capture.
It remains usable without JavaScript through ordinary image links. JavaScript adds
category filters, a keyboard-accessible native dialog, and copy buttons for commands.
Images below the hero load lazily. The entire screenshot set is under 1 MB.

Before publishing, check the page on narrow and wide screens, each gallery filter,
opening and closing an image (including Escape), the copy buttons, and that all
images and links resolve. Keep the install commands in sync with the root README.
