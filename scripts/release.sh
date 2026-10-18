#!/usr/bin/env bash
# Build both release front ends. Run from any directory; requires Bash, Go,
# Git, GNU tar and sha256sum. No tag/ref is created or pushed by this script.
set -euo pipefail

if (( $# < 2 || $# > 3 )); then
    echo 'Usage: bash scripts/release.sh vMAJOR.MINOR.PATCH FULL_COMMIT [OUTPUT_DIR]' >&2
    exit 2
fi
version=$1
commit=$2
if [[ ! $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ || ! $commit =~ ^[0-9a-f]{40}$ ]]; then
    echo 'Expected a stable vMAJOR.MINOR.PATCH version and full lowercase Git commit.' >&2
    exit 2
fi
project=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
out=$(realpath -m -- "${3:-$project/dist}")
mkdir -p -- "$out"
name="touko_${version#v}_linux_amd64"
archive="$out/$name.tar.gz"
checksums="$out/$name.sha256"
if [[ -e $archive || -e $checksums ]]; then
    echo "Refusing to overwrite an existing release in $out" >&2
    exit 1
fi
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
bundle="$work/$name"
mkdir -p "$bundle/bin" "$bundle/share/doc/touko/docs" "$bundle/share/licenses/touko"
cd -- "$project"
export CGO_ENABLED=0 GOOS=linux GOARCH=amd64
ldflags="-s -w -X github.com/Eurekaimer/TOUKO/internal/buildinfo.Version=$version -X github.com/Eurekaimer/TOUKO/internal/buildinfo.Commit=$commit"
go build -trimpath -ldflags "$ldflags" -o "$bundle/bin/touko" ./cmd/touko
go build -trimpath -ldflags "$ldflags" -o "$bundle/bin/touko-desktop" ./cmd/touko-desktop
install -m644 LICENSE "$bundle/share/licenses/touko/LICENSE"
install -m644 README.md README_en.md "$bundle/share/doc/touko/"
install -m644 docs/*.md "$bundle/share/doc/touko/docs/"
# Keep README and documentation image links valid in the extracted bundle.
cp -R docs/images "$bundle/share/doc/touko/docs/"
install -Dm644 packaging/touko-desktop.desktop "$bundle/share/applications/touko-desktop.desktop"
for size in 16 24 32 48 64 128 256; do
    install -Dm644 "internal/desktop/assets/touko-$size.png" "$bundle/share/icons/hicolor/${size}x${size}/apps/touko.png"
done
install -Dm644 internal/desktop/assets/touko-256.png "$bundle/share/pixmaps/touko.png"
tar -czf "$archive" -C "$work" "$name"
(cd -- "$out" && sha256sum "$name.tar.gz" > "$name.sha256")
printf '%s\n' "$archive" "$checksums"
