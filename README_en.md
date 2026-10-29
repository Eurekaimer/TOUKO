<div align="center">

<img src="https://img.eurekaimer.icu/objects/default/sha256/fa/93/fa93a8ca741cfa57253de4eed814435452ee5fce2863932f0112f7e94455041a.webp" alt="TOUKO" width="220" />

# TOUKO

Keep your pictures in a Git repository you own.

[![Go](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![License](https://img.shields.io/badge/license-MIT-9c5238)](LICENSE)
[![Platform](https://img.shields.io/badge/platform-Linux%20%7C%20Windows%20%7C%20macOS-1b2a3d)](#install)

[简体中文](README.md) · [English](README_en.md)

</div>

TOUKO is an image library backed by a Git repository. Image bytes and metadata live in the repository, GitHub hosts it, and jsDelivr serves the default public URLs. An optional Cloudflare R2 mirror can serve the same bytes from your own domain. The local SQLite database is a read cache; delete it and the library rebuilds from the repository.

Uploads go to a staging area first. `commit` writes one Git commit. File names come from content hashes, so identical bytes are stored once. Each asset records its title, tags, collections, MIME type, size and SHA-256, which keeps old images findable and verifiable.

## Features

- Content-addressed images and metadata, published as one atomic commit
- Separate staging, commit and sync steps; `rm` only removes from the latest tree, Git history stays
- Multi-repository galleries, each with its own staging and publication flow
- Web and desktop interface: drag or paste uploads, search, filters by tag, collection and directory, Markdown copy
- Optional Cloudflare R2 mirror, written only after GitHub confirms the publication
- Settings panel for interface and font scaling, Cloudflare configuration and locally stored R2 keys
- Collapsible tags and collections, 12 entries per page

## Install

Builds cover Linux (amd64/arm64), Windows amd64 and macOS (amd64/arm64). Download `v0.2.0` from [Releases](https://github.com/Eurekaimer/TOUKO/releases), verify the `.sha256`, then unpack.

Linux:

```sh
sha256sum -c touko_0.2.0_linux_amd64.sha256
tar -xzf touko_0.2.0_linux_amd64.tar.gz
sudo install -m755 touko_0.2.0_linux_amd64/bin/touko touko_0.2.0_linux_amd64/bin/touko-desktop /usr/local/bin/
sudo cp -R touko_0.2.0_linux_amd64/share/. /usr/local/share/
touko version
```

On Windows, unpack the `.zip` and run `bin\touko-desktop.exe`; it needs the WebView2 Runtime. On macOS, copy `TOUKO.app` to Applications; if Gatekeeper blocks the first launch, open it from the context menu.

The Linux desktop needs GTK3, WebKitGTK 4.1 and a tray host. Browser-based GitHub login needs [GitHub CLI](https://cli.github.com/) installed once. Both programs need Git 2.46 or newer.

Build from source:

```sh
git clone https://github.com/Eurekaimer/TOUKO.git
cd TOUKO
go build -trimpath -o touko ./cmd/touko
go build -trimpath -o touko-desktop ./cmd/touko-desktop
```

## Quick start

```sh
touko repo use Eurekaimer/TOUKO-WEBP-01
touko sync
touko add photo.png --tag travel --collection demo
touko commit -m "images: add a photo"
touko find travel
touko url <UUID> --provider jsdelivr
```

`touko --help` lists the commands: `add`, `edit`, `rm`, `ls`, `find`, `show`, `status`, `commit`, `sync`, `verify`, `reindex`, `import`, `repo`, `auth`, `serve`, `url`, `cloudflare`, `reset`, `version`. All accept `--json`.

## Interface

```sh
touko serve --port 7788        # browser; add --no-open to skip opening it
touko-desktop                  # native window
```

The server listens on `127.0.0.1` and checks Host, Origin and CSRF. GitHub credentials stay inside the server process.

The gear button opens settings:

- Display: interface and font scaling
- Cloudflare R2: enable switch, endpoint, bucket, public base URL and keys
- Keys are stored in the workspace state directory at `cloudflare/credentials.json` (directory 0700, file 0600). The form fills them back masked; press Show to read them

Saving settings does not upload or sync anything. Turning the mirror off keeps the saved addresses.

## Optional Cloudflare R2

jsDelivr remains the default provider. R2 is an optional mirror configured in settings or directly in the workspace `config.json`:

```json
"cloudflare": {
  "enabled": true,
  "endpoint": "https://<ACCOUNT_ID>.r2.cloudflarestorage.com",
  "bucket": "touko-imgs",
  "public_base_url": "https://img.eurekaimer.icu"
}
```

`endpoint` is an HTTPS origin without a path; the region is fixed to `auto`. `public_base_url` is never inferred. Credentials come from the settings panel or two environment variables, not from `.env`, AWS profiles or gh:

```sh
export TOUKO_R2_ACCESS_KEY_ID=...
export TOUKO_R2_SECRET_ACCESS_KEY=...
```

New images upload after GitHub confirms the publication. Existing galleries and failed jobs use:

```sh
touko cloudflare sync --dry-run
touko cloudflare sync
touko url <UUID> --provider cloudflare
```

A Cloudflare URL is returned only for a synced receipt that still matches a live HEAD request. Staged images cannot be copied. Domain binding, DNS and cache settings belong to Cloudflare.

## Performance

The four-way remote comparison using 200 real images is incomplete. The newer
run continued after failures, so later commits may contain several staged images
and cannot be treated as valid single-image measurements.

| Version / mirror | Observed result | Mean per image |
| --- | --- | ---: |
| Old / GitHub only | 196 successful, 1 failed; one attempt | 8,796.2 ms |
| Old / GitHub + R2 | Same-corpus run incomplete | — |
| New / GitHub only | 193 records, 10 failures, then stopped | Not valid |
| New / GitHub + R2 | Same-corpus run incomplete | — |

Three passes over the same images measured **0.111 ms/image** for one in-memory
SHA-256 pass, excluding disk, conversion and network. Historical single-image
request counts, methodology and limits are in [Benchmark](docs/benchmark.md).
The current client adds a pre-publication HEAD read.

**Migration safety:** The published `touko migrate paths` and `--prune-old`
have not passed failure-recovery and legacy-object safety checks. Do not use
them on a production gallery until the fix is complete.

## Documentation

[CLI](docs/usage.md) · [Webview](docs/webview.md) · [Architecture](docs/code-guide.md) · [Function reference](docs/function-reference.md) · [Repository format](docs/repository-format.md) · [Local state](docs/local-state.md) · [Publication](docs/publication.md) · [Benchmark](docs/benchmark.md)

## Boundaries

One image can be up to 100 MB. WebP conversion only replaces the original when the result is smaller; GIF and WebP inputs keep their bytes. Previews are not a thumbnail service. Local repositories and private GitHub repositories do not produce public URLs unless Cloudflare is enabled explicitly. `rm` does not rewrite Git history.

The Linux/Wayland interface has been checked by hand. Windows and macOS builds pass CLI smoke tests; their GUI, tray and clipboard paths are still unverified on real desktops.

## Credits

The name comes from Touko Nanami in *Yagate Kimi ni Naru*. Monaspace Radon, LXGW WenKai, Quicksand and Playfair Display are licensed under SIL OFL. The native window comes from [MyGo](https://github.com/egoist/mygo).

## License

[MIT](LICENSE)
