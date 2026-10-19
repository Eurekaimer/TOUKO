#!/usr/bin/env bash
# Build one release bundle for the target selected by GOOS/GOARCH, which
# default to the host. Supported targets are linux/{amd64,arm64} (tar.gz,
# desktop entry and icons), windows/amd64 (portable zip) and
# darwin/{amd64,arm64} (tar.gz with a real .app). The archive root is
# touko_VERSION_GOOS_GOARCH and every archive has a matching .sha256 file in
# sha256sum format. Requires Bash, Go, Git, Python 3.12+, cp, mktemp and chmod;
# no tag/ref is created or pushed by this script.
set -euo pipefail

if (( $# < 2 || $# > 3 )); then
    echo 'Usage: GOOS=... GOARCH=... bash scripts/release.sh vMAJOR.MINOR.PATCH FULL_COMMIT [OUTPUT_DIR]' >&2
    exit 2
fi
version=$1
commit=$2
if [[ ! $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ || ! $commit =~ ^[0-9a-f]{40}$ ]]; then
    echo 'Expected a stable vMAJOR.MINOR.PATCH version and full lowercase Git commit.' >&2
    exit 2
fi

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
project=$(cd -- "$script_dir/.." && pwd -P)
goos=${GOOS:-$(go env GOHOSTOS)}
goarch=${GOARCH:-$(go env GOHOSTARCH)}
case "$goos/$goarch" in
    linux/amd64|linux/arm64|windows/amd64|darwin/amd64|darwin/arm64) ;;
    *)
        echo "Unsupported target $goos/$goarch; supported: linux/{amd64,arm64}, windows/amd64, darwin/{amd64,arm64}." >&2
        exit 2
        ;;
esac

if command -v python3 >/dev/null 2>&1; then
    python=python3
elif command -v python >/dev/null 2>&1; then
    python=python
else
    echo 'Python 3 is required to assemble the archive and checksum.' >&2
    exit 1
fi
if ! "$python" -c 'import sys; raise SystemExit(sys.version_info < (3, 12))' >/dev/null 2>&1; then
    echo "$python is not Python 3.12 or newer." >&2
    exit 1
fi

# mkdir resolves a relative output path without GNU realpath -m, which macOS
# does not have. Everything below uses the absolute result.
out_arg=${3:-$project/dist}
mkdir -p "$out_arg"
out=$(cd -- "$out_arg" && pwd -P)

base_version=${version#v}
name="touko_${base_version}_${goos}_${goarch}"
if [[ $goos == windows ]]; then
    suffix=.zip
else
    suffix=.tar.gz
fi
archive="$out/$name$suffix"
checksums="$out/$name.sha256"
if [[ -e $archive || -L $archive || -e $checksums || -L $checksums ]]; then
    echo "Refusing to overwrite an existing release in $out" >&2
    exit 1
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# One small standard-library helper covers the pieces that are awkward in
# portable shell: an ICNS wrapper around the committed 256px PNG, the app's
# Info.plist, and tar/zip archives with a sha256sum-compatible checksum.
cat > "$work/release-pack.py" <<'PY'
import hashlib
import os
from pathlib import Path
import plistlib
import shutil
import struct
import sys
import tarfile
import time
import zipfile

PNG_SIGNATURE = b"\x89PNG\r\n\x1a\n"


def fail(message):
    raise SystemExit(message)


def write_icns(png_path, icns_path):
    png = Path(png_path).read_bytes()
    if len(png) < 24 or png[:8] != PNG_SIGNATURE:
        fail(f"{png_path} is not a PNG file")
    width, height = struct.unpack(">II", png[16:24])
    if (width, height) != (256, 256):
        fail(f"{png_path} is {width}x{height}, but the ic08 entry needs 256x256")
    entry = b"ic08" + struct.pack(">I", len(png) + 8) + png
    Path(icns_path).write_bytes(b"icns" + struct.pack(">I", len(entry) + 8) + entry)


def write_app(app_path, version, png_path):
    app = Path(app_path)
    contents = app / "Contents"
    (contents / "MacOS").mkdir(parents=True, exist_ok=True)
    (contents / "Resources").mkdir(parents=True, exist_ok=True)
    write_icns(png_path, contents / "Resources" / "touko.icns")
    plist = {
        "CFBundleName": "TOUKO",
        "CFBundleDisplayName": "TOUKO",
        "CFBundleIdentifier": "com.eurekaimer.touko",
        "CFBundleVersion": version,
        "CFBundleShortVersionString": version,
        "CFBundleExecutable": "touko-desktop",
        "CFBundlePackageType": "APPL",
        "CFBundleInfoDictionaryVersion": "6.0",
        "CFBundleDevelopmentRegion": "en",
        "CFBundleIconFile": "touko.icns",
        "LSMinimumSystemVersion": "12.0",
        "LSApplicationCategoryType": "public.app-category.graphics-design",
        "NSPrincipalClass": "NSApplication",
        "NSHighResolutionCapable": True,
        "NSSupportsAutomaticGraphicsSwitching": True,
    }
    with open(contents / "Info.plist", "wb") as stream:
        plistlib.dump(plist, stream, fmt=plistlib.FMT_XML, sort_keys=True)
    (contents / "PkgInfo").write_bytes(b"APPL????")


def members(source):
    for dirpath, dirnames, filenames in os.walk(source):
        dirnames.sort()
        for filename in sorted(filenames):
            path = Path(dirpath) / filename
            yield path, path.relative_to(source).as_posix()


def member_mode(relative):
    executable = (relative.startswith("bin/")
                  or relative.startswith("TOUKO.app/Contents/MacOS/"))
    return 0o755 if executable else 0o644


def write_tar(source, root_name, destination):
    with tarfile.open(destination, "w:gz", format=tarfile.PAX_FORMAT) as archive:
        for path, relative in members(source):
            info = archive.gettarinfo(str(path), arcname=root_name + "/" + relative)
            info.mode = member_mode(relative)
            info.uid = info.gid = 0
            info.uname = info.gname = ""
            with path.open("rb") as stream:
                archive.addfile(info, stream)


def write_zip(source, root_name, destination):
    with zipfile.ZipFile(destination, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
        for path, relative in members(source):
            date_time = time.localtime(path.stat().st_mtime)[:6]
            if date_time[0] < 1980:
                date_time = (1980, 1, 1, 0, 0, 0)
            info = zipfile.ZipInfo(root_name + "/" + relative, date_time=date_time)
            info.compress_type = zipfile.ZIP_DEFLATED
            info.create_system = 3
            info.external_attr = (0o100000 | member_mode(relative)) << 16
            with path.open("rb") as source_file, archive.open(info, "w") as target_file:
                shutil.copyfileobj(source_file, target_file)


def sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def write_checksum(archive_path):
    archive = Path(archive_path)
    checksum = archive.with_name(archive.name.removesuffix(".zip").removesuffix(".tar.gz") + ".sha256")
    with open(checksum, "w", encoding="ascii", newline="\n") as stream:
        stream.write(sha256(archive) + "  " + archive.name + "\n")


def main():
    if len(sys.argv) < 2:
        fail("usage: release-pack.py app|archive ...")
    command = sys.argv[1]
    if command == "app":
        if len(sys.argv) != 5:
            fail("usage: release-pack.py app APP_DIR VERSION ICON_PNG")
        write_app(sys.argv[2], sys.argv[3], sys.argv[4])
    elif command == "archive":
        if len(sys.argv) != 6:
            fail("usage: release-pack.py archive tar.gz|zip SOURCE ROOT_NAME DEST")
        kind, source, root_name, destination = sys.argv[2:]
        source = Path(source)
        if not source.is_dir():
            fail(f"{source} is not a directory")
        if kind == "tar.gz":
            write_tar(source, root_name, destination)
        elif kind == "zip":
            write_zip(source, root_name, destination)
        else:
            fail(f"unsupported archive kind {kind}")
        write_checksum(destination)
    else:
        fail(f"unknown command {command}")


if __name__ == "__main__":
    main()
PY

bundle="$work/$name"
mkdir -p "$bundle/bin" "$bundle/share/doc/touko/docs" "$bundle/share/licenses/touko"
exe=
if [[ $goos == windows ]]; then
    exe=.exe
fi

export CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch"
cd -- "$project"

common_ldflags="-s -w -X github.com/Eurekaimer/TOUKO/internal/buildinfo.Version=$version -X github.com/Eurekaimer/TOUKO/internal/buildinfo.Commit=$commit"
# Match MyGo's production build: no UI inspector, production mode, and the
# package metadata its Linux/Windows helpers would normally supply.
desktop_ldflags="$common_ldflags -X github.com/egoist/mygo.production=1"
desktop_ldflags="$desktop_ldflags -X github.com/egoist/mygo.packageName=TOUKO"
desktop_ldflags="$desktop_ldflags -X github.com/egoist/mygo.packageVersion=$base_version"
desktop_ldflags="$desktop_ldflags -X github.com/egoist/mygo.packageIdentifier=com.eurekaimer.touko"
if [[ $goos == windows ]]; then
    desktop_ldflags="$desktop_ldflags -H=windowsgui"
fi

echo "Building touko $version ($commit) for $goos/$goarch"
go build -trimpath -ldflags "$common_ldflags" -o "$bundle/bin/touko$exe" ./cmd/touko

if [[ $goos == darwin ]]; then
    app="$bundle/TOUKO.app"
    mkdir -p "$app/Contents/MacOS"
    desktop="$app/Contents/MacOS/touko-desktop"
else
    desktop="$bundle/bin/touko-desktop$exe"
fi
go build -trimpath -tags mygo_noinspector -ldflags "$desktop_ldflags" -o "$desktop" ./cmd/touko-desktop
chmod 0755 "$bundle/bin/touko$exe" "$desktop"

if [[ $goos == darwin ]]; then
    "$python" "$work/release-pack.py" app "$app" "$base_version" \
        "$project/internal/desktop/assets/touko-256.png"
    if [[ $(uname -s) == Darwin ]] && command -v codesign >/dev/null 2>&1; then
        # Ad-hoc signature only on a real macOS host; never claim one elsewhere.
        codesign --force --sign - "$app"
    fi
fi

cp LICENSE "$bundle/share/licenses/touko/LICENSE"
cp README.md README_en.md "$bundle/share/doc/touko/"
cp docs/*.md "$bundle/share/doc/touko/docs/"
# Keep README and documentation image links valid in the extracted bundle.
cp -R docs/images "$bundle/share/doc/touko/docs/"

if [[ $goos == linux ]]; then
    mkdir -p "$bundle/share/applications"
    cp packaging/touko-desktop.desktop "$bundle/share/applications/touko-desktop.desktop"
    for size in 16 24 32 48 64 128 256; do
        icon_dir="$bundle/share/icons/hicolor/${size}x${size}/apps"
        mkdir -p "$icon_dir"
        cp "internal/desktop/assets/touko-$size.png" "$icon_dir/touko.png"
    done
    mkdir -p "$bundle/share/pixmaps"
    cp internal/desktop/assets/touko-256.png "$bundle/share/pixmaps/touko.png"
fi

if [[ $goos == windows ]]; then
    archive_kind=zip
else
    archive_kind=tar.gz
fi
"$python" "$work/release-pack.py" archive "$archive_kind" "$bundle" "$name" "$archive"

byte_size() {
    wc -c < "$1" | tr -d '[:space:]'
}
report_size() {
    printf '  %-34s %10s bytes\n' "$2" "$(byte_size "$1")"
}
echo 'Release sizes:'
report_size "$bundle/bin/touko$exe" "bin/touko$exe"
if [[ $goos == darwin ]]; then
    report_size "$desktop" "TOUKO.app/Contents/MacOS/touko-desktop"
else
    report_size "$desktop" "bin/touko-desktop$exe"
fi
report_size "$archive" "$(basename "$archive")"
report_size "$checksums" "$(basename "$checksums")"

printf '%s\n' "$archive" "$checksums"
