# TOUKO
TOUKO treats image management as tree-oriented repository operations instead of individual file uploads.

## CLI

Requires Go 1.26+ and Git with SHA-1 repository support. No CGO required.

```sh
go build -o /tmp/touko ./cmd/touko
/tmp/touko --state-dir /tmp/my-gallery-state repo init --local /tmp/my-gallery.git
/tmp/touko --state-dir /tmp/my-gallery-state add picture.png --namespace blog --tag demo
/tmp/touko --state-dir /tmp/my-gallery-state status --json
/tmp/touko --state-dir /tmp/my-gallery-state ls
```

State flags may follow command arguments. `add - --name 剪贴板.png` reads one image from stdin. `edit ID --title TITLE --tag TAG` replaces supplied fields only; `--clear-tags` and `--clear-collections` explicitly clear lists. `rm ID...` stages atomic removal; `reset --staged` discards unpublished changes.

Another device selects the existing repository with `repo use --local /absolute/gallery.git`, then `sync`. Each device needs its own state directory. Sync validates a fixed Git tree before atomically replacing the SQLite cache; corrupt metadata never produces a partial gallery.

See [repository format](docs/repository-format.md) and [local state](docs/local-state.md).
