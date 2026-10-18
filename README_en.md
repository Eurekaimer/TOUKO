<div align="center">

<img src="https://socialify.dev/Eurekaimer/TOUKO/image?custom_language=Go&font=Raleway&forks=1&language=1&logo=https%3A%2F%2Fraw.githubusercontent.com%2FEurekaimer%2FTOUKO-IMGBED-03%2Fmain%2Fobjects%2Fdefault%2Fsha256%2Fa6%2Fe0%2Fa6e002cde06d25d106a9e80bafcc7762003491f4a2bf4f5bc2a8332d5411c6b0.png&name=1&owner=1&pattern=Circuit+Board&pulls=1&stargazers=1&theme=Light" alt="TOUKO" width="640" />

# TOUKO

**Keep your pictures in your own Git repository.**

Content-addressed · Git-style staging · Local-first reads · Multi-repository libraries

[![Go](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![License](https://img.shields.io/badge/license-MIT-9c5238)](LICENSE)
[![Platform](https://img.shields.io/badge/platform-Linux%20%C2%B7%20macOS%20%C2%B7%20Windows-1b2a3d)](#install)
[![Docs](https://img.shields.io/badge/docs-7%20guides-7f3f59)](docs/webview.md)

[English](README_en.md) · [简体中文](README.md)

</div>

---

TOUKO is an image library that uses a Git repository as its database. Instead of handing your pictures to a hosting service, it writes every image and every piece of metadata into **a repository you own** — GitHub is the single source of truth, and jsDelivr is only an optional delivery layer.

## The name

TOUKO is named after **Nanami Touko** from *Bloom Into You* (やがて君になる).

> 好きって、なんだろう。
> 好きって、なに？
>
> — Nanami Touko

She spends the whole story asking what "liking" something means, and who she is. An image library asks the same kind of question: who owns a picture, where does it live, who may fetch it, and does deleting it mean it never existed? TOUKO hands those answers to Git — every change is recorded, and every file is named after its own content.

## Why not another PicGo

PicGo and PicList solve "upload, then copy the link". TOUKO solves "in a year, can I still find this picture and know it is the exact bytes I uploaded".

| | Conventional uploaders | TOUKO |
| --- | --- | --- |
| State | Remote folder + local config | The Git repository itself, fully reconstructible |
| Deduplication | Usually none | Content addressing; identical bytes share one object |
| Publishing | Upload per file | Stage first, publish as one commit, keep the scene on failure |
| New machine | Reconfigure | `repo use` + `sync` restores everything |
| Deletion | Delete a file | Defined deletion, history still auditable |
| Scale | One repository | One logical library across repositories, spread by capacity |

## Features

- **The repository is the database.** Assets, objects, titles, tags, collections, MIME types, dimensions and content hashes live remotely. Local SQLite is only a rebuildable cache.
- **Content addressing.** A file is named by the SHA-256 of its bytes. Identical pictures deduplicate naturally, and a link can never point at an overwritten image.
- **Stage, then publish.** Uploads, edits and removals enter a staging area; only `commit` writes a Git commit. A failure never leaves half a gallery behind.
- **Local-first reads.** Once a local shallow clone exists, previews do not contact the remote. Creating the first GitHub clone still needs a network connection; `sync` explicitly refreshes it.
- **Unified libraries.** Merge several repositories into one browsing and upload surface. Each repository keeps its own staging, commits and recovery, and uploads are assigned by remaining capacity.
- **Real deletion.** Removing a picture takes it out of the latest tree and verifies the result; Git history remains, which is a feature rather than an oversight.
- **CLI + native desktop.** Written in Go with no Node build step or runtime CDN. `touko` CLI/Web and the MyGo `touko-desktop` window share the HTTP surface and business logic; a release bundle contains both executables.
- **Formats and space.** Accepts PNG, JPEG, GIF and WebP. Uploads can opt into PNG/JPEG → WebP conversion, which replaces the original only when the encoded bytes are smaller. GIF and existing WebP inputs are never re-encoded.
- **Large galleries stay interactive.** Cards mount in bounded batches and offscreen layout is skipped; filtering, scrolling and selecting remain responsive with 1000 images (method in the [benchmark guide](docs/benchmark.md)).

## Interface

<div align="center">
  <img src="docs/images/webview-gallery.png" alt="TOUKO gallery interface" width="820" />
</div>

Cream paper and deep navy, LXGW WenKai and Monaspace Radon. A single click on a card copies its Markdown and reports the result beside the pointer. Drag and drop, clipboard paste, filtering by collection, tag, directory or content hash, and per-repository source filtering are all supported.

## Install

### Platforms and runtime dependencies

| Platform | v0.1.0 status |
| --- | --- |
| Linux amd64 | Verified; the release pipeline builds both CLI and desktop |
| macOS | Experimental / unverified; no artifact in this release matrix |
| Windows | Experimental / unverified; no artifact in this release matrix |

The Linux desktop has been exercised on Wayland with GTK3 and WebKitGTK 4.1. This does not claim compatibility with every distribution, desktop environment or architecture. Both executables require Git with SHA-1 repository and `show-ref --exists` support (Git 2.46+ recommended). The desktop also needs GTK3 and WebKitGTK 4.1; the tray needs Ayatana AppIndicator and a tray host. Install `gh` for the recommended GitHub login flow.

### Release installation (after publication)

This checkout prepares v0.1.0 infrastructure; it does not mean a tag or Release has been published. Once available, download `touko_0.1.0_linux_amd64.tar.gz` and its `.sha256` file from [GitHub Releases](https://github.com/Eurekaimer/TOUKO/releases), then run in the download directory:

```sh
sha256sum -c touko_0.1.0_linux_amd64.sha256
tar -xzf touko_0.1.0_linux_amd64.tar.gz
sudo install -m755 touko_0.1.0_linux_amd64/bin/touko touko_0.1.0_linux_amd64/bin/touko-desktop /usr/local/bin/
sudo cp -R touko_0.1.0_linux_amd64/share/. /usr/local/share/
touko version
touko-desktop --version
```

Prebuilt artifacts do not require Go. You can also run the extracted `bin/touko` or `bin/touko-desktop` directly; desktop entries and icons need installation into XDG locations for system menu discovery. A missing system WebView makes the desktop fail explicitly; the CLI remains independently usable.

### Build from source

Use the Go version specified by `go.mod` (currently 1.27.1) and the runtime dependencies above:

```sh
git clone https://github.com/Eurekaimer/TOUKO.git
cd TOUKO
go build -o touko ./cmd/touko
go build -o touko-desktop ./cmd/touko-desktop
./touko version --json
```

Ordinary development builds report `dev / unknown`, not a release version. See [versioning and releases](docs/usage.md#11-版本与发布) for metadata injection and publication, and the [benchmark guide](docs/benchmark.md) for reproducible measurements.

Arch users can build the local package from their current checkout (not a published AUR package); it installs both programs, the desktop entry and icons:

```sh
cd packaging && makepkg -si
```

## Quick start

```sh
# Connect an existing repository, or run repo init first
./touko --state-dir ~/.local/share/touko repo use Eurekaimer/TOUKO-IMGBED-01
./touko --state-dir ~/.local/share/touko sync

# Add a picture, organise its metadata, publish
./touko --state-dir ~/.local/share/touko add photo.png --tag travel --collection demo
./touko --state-dir ~/.local/share/touko commit -m "images: add a photo"

# Find it again by title, filename, tag, collection or content hash
./touko --state-dir ~/.local/share/touko find travel
./touko --state-dir ~/.local/share/touko find 4aff401b
```

Other commands: `status`, `ls`, `find`, `show`, `edit`, `rm`, `verify`, `reindex`, `import`. All of them accept `--json`.

## Web interface

```sh
./touko --state-dir ~/.local/share/touko serve --port 7788
```

The browser opens by default; `--no-open` starts the server alone. `touko-desktop` opens the same interface in a native window without an address bar, still rendered by the system WebView.

The server listens on loopback only and validates Host, Origin and CSRF. GitHub credentials stay inside the server process and never reach the page.

## Galleries and CDN

The GitHub repository is the source; jsDelivr is an optional delivery layer:

```
https://cdn.jsdelivr.net/gh/OWNER/REPO@BRANCH/objects/<ns>/sha256/<aa>/<bb>/<hash>.<ext>
https://raw.githubusercontent.com/OWNER/REPO/BRANCH/objects/<ns>/sha256/<aa>/<bb>/<hash>.<ext>
```

jsDelivr enforces a size limit on GitHub packages and answers 403 for the whole package once it is exceeded. Fall back to the raw URL, which serves the same bytes; TOUKO spreads pictures across repositories by capacity and records which repository actually holds each asset.

## Documentation

| Guide | Contents |
| --- | --- |
| [Webview](docs/webview.md) | Startup, security boundary, live GitHub acceptance |
| [CLI](docs/usage.md) | From the demo to multiple devices, recovery and JSON |
| [Architecture](docs/code-guide.md) | Module boundaries, call chains and trade-offs |
| [Function reference](docs/function-reference.md) | Inputs, outputs and reasons for every production function |
| [Repository format](docs/repository-format.md) | Remote layout and versioning |
| [Local state](docs/local-state.md) | SQLite cache, durable staging and rebuilds |
| [Publication](docs/publication.md) | Commit, recovery and deletion semantics |
| [Benchmark](docs/benchmark.md) | Reproducible CLI/HTTP baseline and desktop RSS method |

## Boundaries

Previews are not a thumbnail service, and local or private repositories produce no public links. `rm` removes a picture from the latest tree while Git history keeps it; truly erasing it requires rewriting history, which TOUKO does not offer. Only Linux/Wayland is verified today; the Windows and macOS clipboard paths follow the MyGo contract but have not been run on those systems.

## Credits

Typefaces Monaspace Radon, LXGW WenKai, Quicksand and Playfair Display (all SIL OFL). The native window is provided by [MyGo](https://github.com/egoist/mygo). Visual review followed Anthropic's `frontend-design` guidance.

## License

[MIT](LICENSE)
