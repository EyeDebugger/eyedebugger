# The project site

`index.html` is the whole site at <https://eyedbg.izzat.dev>: one static page, its CSS and
JavaScript inline, no build step and no dependencies. `.github/workflows/pages.yml` publishes this
directory (everything but this README) to GitHub Pages on every push to `main` that touches it.

| File | What it is |
|---|---|
| `index.html` | The page. |
| `og.png` | The 1200×630 image shown when a link to the site is shared. |
| `apple-touch-icon.png` | The 180×180 home-screen icon. The favicon is an inline SVG in `index.html`. |
| `media/` | The demo video and screenshots, once they exist (below). |

## Preview

Any static file server works:

```sh
python3 -m http.server 8000 --directory site    # then open http://localhost:8000
```

Check both themes (the toggle, or your system setting) and a narrow window. With
"reduce motion" on in your OS, every animation settles at once and the hero shows its last step.

## The demo video and screenshots

The "See it in action" section has a video slot and three screenshot slots. Until a source is
set, each shows a placeholder. To publish one, add the file to `site/media/` (or host it
elsewhere) and set the slot's attribute in `index.html`:

| Slot | Attribute | Suggested file |
|---|---|---|
| Video | `data-video` on `.video-slot` | `media/demo.mp4`: H.264, 1920×1080, under ~50 MB |
| Video poster | `data-poster` on `.video-slot` | `media/demo-poster.webp`: 1920×1080 |
| Terminal screenshot | `data-shot` on the first `.shot` | `media/terminal.webp`: 1600×1000 (16:10) |
| VS Code screenshot | `data-shot` on the second `.shot` | `media/vscode.webp`: 1600×1000 |
| .NET views screenshot | `data-shot` on the third `.shot` | `media/dotnet.webp`: 1600×1000 |

For example:

```html
<div class="video-slot" data-video="media/demo.mp4" data-poster="media/demo-poster.webp" …>
<figure class="shot" data-shot="media/vscode.webp" …>
```

- `data-video` also takes an `https://` URL (a large video is better on a CDN than in git: GitHub
  refuses files over 100 MB) or a YouTube link, which plays through `youtube-nocookie.com`.
- Screenshots are cropped to 16:10 from the top left; clicking one opens it full size.
- Keep each figure's `data-alt` in step with what its image shows.
- Screenshots and recordings must not show secrets or private data from the debugged program
  (AGENTS.md rule 9).

## Content

- The terminal output on the page follows what `eyedbg` really prints (`internal/cli/testdata/*.golden`
  and the commands' help). When an output format changes, update the page with it.
- Only code is monospace; everything else is RznSans.
- RznSans comes from our font CDN's `rznsans/latest`, which RznType's deploy script overwrites
  with every release: a new release reaches the site once the CDN's cache expires (4 hours), with
  no change here. Don't pin a `v<rev>`, and don't preload the `.woff2` files: the stylesheet's
  font URLs carry the release in `?v=`, so a preload would stop matching after the next one.

## Deployment

Pages is set up in the repository settings: source "GitHub Actions", custom domain
`eyedbg.izzat.dev`, HTTPS enforced. The `github-pages` environment accepts deployments from `main`
only. To redeploy without a change: `gh workflow run pages.yml`.
