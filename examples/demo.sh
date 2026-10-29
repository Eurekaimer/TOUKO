#!/usr/bin/env bash
# Run from the project root. Requires Go matching go.mod, Git, Bash, Python 3 stdlib.
# This demo uses only local Git; it never reads a token or contacts GitHub.
set -euo pipefail

if [[ ${1:-} == --help || ${1:-} == -h ]]; then
    printf '%s\n' 'Usage: bash examples/demo.sh [EMPTY_DIRECTORY]' \
        'Run from the TOUKO project root; requires Go matching go.mod, Git, Bash, Python 3 (stdlib only).' \
        'Go module dependencies must already be cached; network downloads are disabled.' \
        'An omitted directory uses mktemp. Results are retained; nonempty targets are refused.'
    exit 0
fi
if (( $# > 1 )); then
    printf '%s\n' 'Usage: bash examples/demo.sh [EMPTY_DIRECTORY]' >&2
    exit 2
fi
for dependency in go git python3; do
    if ! command -v "$dependency" >/dev/null 2>&1; then
        printf 'Missing dependency: %s\n' "$dependency" >&2
        exit 1
    fi
done
if [[ ! -f go.mod || ! -d cmd/touko || ! -d examples/images ]]; then
    printf '%s\n' 'Run this script from the TOUKO project root.' >&2
    exit 1
fi
if (( $# == 1 )); then
    root=$1
    if [[ -z $root ]]; then
        printf '%s\n' 'EMPTY_DIRECTORY must not be empty text.' >&2
        exit 2
    fi
else
    root=$(mktemp -d "${TMPDIR:-/tmp}/touko-demo.XXXXXXXX")
fi
# Keep the build offline too: do not fetch modules or a newer Go toolchain.
export GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GONOPROXY=none

python3 - "$root" <<'PY'
import json
from pathlib import Path
import shlex
import subprocess
import sys
import uuid

project = Path.cwd()
root = Path(sys.argv[1]).absolute()
# Refuse before building, creating state, or changing any existing contents.
if root.exists():
    if not root.is_dir() or next(root.iterdir(), None) is not None:
        sys.exit(f'Refusing nonempty or non-directory target: {root}')
else:
    root.mkdir(parents=True)
root = root.resolve()
binary = root / 'touko'
gallery = root / 'gallery.git'
images = project / 'examples' / 'images'
a, b, c = (root / f'device-{name}' for name in 'abc')


def require(condition, message):
    """Fail an observable demo invariant even when Python optimization is enabled."""
    if not condition:
        raise RuntimeError(message)


def run(argv, *, data=None, expected=0):
    """Run a real subprocess and retain diagnostics when its exit status differs."""
    result = subprocess.run([str(part) for part in argv], input=data,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    require(result.returncode == expected,
            f'{shlex.join([str(part) for part in argv])}: expected exit {expected}, '
            f'got {result.returncode}\nstdout: {result.stdout.decode(errors="replace")}\n'
            f'stderr: {result.stderr.decode(errors="replace")}')
    return result


def cli(device, *args, data=None, expected=0):
    """Start a fresh CLI process against one device's persistent state."""
    return run([binary, '--state-dir', device, *args], data=data, expected=expected)


def document(device, *args, expected=0):
    """Decode the CLI JSON contract, including structured errors on stderr."""
    result = cli(device, *args, '--json', expected=expected)
    if expected:
        require(not result.stdout, 'A failed JSON command wrote to stdout')
        return json.loads(result.stderr)
    return json.loads(result.stdout)


def git(*args):
    """Inspect the actual bare Git repository rather than CLI terminal wording."""
    return run(['git', '--git-dir', gallery, *args]).stdout.decode().strip()


def head():
    """Read the published main ref independently of the local index."""
    return git('rev-parse', 'refs/heads/main')


def tree_entries():
    """Map every current immutable tree path to its Git blob SHA."""
    entries = {}
    for row in filter(None, git('ls-tree', '-r', '-z', head()).split('\0')):
        meta, path = row.split('\t', 1)
        entries[path] = meta.split()[2]
    return entries


def paths():
    """Read all current immutable tree paths using Git's NUL-delimited output."""
    return set(tree_entries())


def tree_counts(metadata, objects, blobs):
    """Require exact metadata, placement and distinct-blob counts at the latest HEAD."""
    current = tree_entries()
    require(len([p for p in current if p.startswith('.touko/assets/')]) == metadata,
            f'Expected {metadata} asset metadata paths')
    object_paths = [p for p in current if p.startswith('objects/')]
    require(len(object_paths) == objects, f'Expected {objects} object paths')
    require(len({current[p] for p in object_paths}) == blobs,
            f'Expected {blobs} distinct object blobs')


def assets(device):
    """Compare complete persisted Asset records keyed by independent UUID identity."""
    views = document(device, 'ls')['assets']
    require(all(view['state'] == 'committed' for view in views),
            'Expected only committed Asset views')
    return {view['asset']['id']: view['asset'] for view in views}


def clean(device):
    """Assert no unpublished changes remain on this device."""
    require(document(device, 'status')['changes'] == [], 'Expected clean staging')


def verify(device, asset_count, object_count):
    """Check the deep verifier's exact published head and object counts."""
    require(document(device, 'verify') == {
        'head': head(), 'assets': asset_count, 'objects': object_count,
    }, 'Deep verify result differs from the published gallery')


def placement(asset):
    """Require the asset-id path recorded in metadata and return it."""
    expected = f'objects/{asset["namespace"]}/{asset["id"]}.{asset["object"]["ext"]}'
    require(asset.get('path') == expected,
            f'Asset {asset["id"]} must own {expected!r}, got {asset.get("path")!r}')
    return expected


def filesystem_state():
    """Detect content-entry or modification-time changes during target refusal."""
    return {str(path.relative_to(root)): (path.lstat().st_mode,
            path.lstat().st_size, path.lstat().st_mtime_ns)
            for path in [root, *root.rglob('*')]}


def main():
    """Exercise publication, reconstruction, stale deletion, dedup, and stdin via CLI."""
    print(f'Building CLI; retaining demo at {root}', flush=True)
    run(['go', 'build', '-o', binary, './cmd/touko'])
    document(a, 'repo', 'init', '--local', str(gallery))
    initial_head = head()
    initial_count = int(git('rev-list', '--count', initial_head))
    specifications = [
        ('red.png', 'demo red one', 'demo-red-one'),
        ('red.png', 'demo red two', 'demo-red-two'),
        ('blue.png', 'demo blue', 'demo-blue'),
        ('green.png', 'demo green', 'demo-green'),
    ]
    ids = []
    for filename, title, tag in specifications:
        asset_id = cli(a, 'add', str(images / filename), '--namespace', 'blog',
                       '--title', title, '--tag', tag, '--collection', 'demo-gallery',
                       '--output', 'id').stdout.decode().strip()
        require(str(uuid.UUID(asset_id)) == asset_id, 'add --output id did not return a UUID')
        ids.append(asset_id)
    require(len(set(ids)) == 4, 'Repeated content must still create independent Asset IDs')
    red1, red2, blue, green = ids
    status = document(a, 'status')
    require(status['head'] == initial_head and status['base_head'] == initial_head,
            'Staging changed the committed head/base')
    require(len(status['changes']) == 4 and
            {change['asset_id'] for change in status['changes']} == set(ids) and
            all(change['kind'] == 'add' for change in status['changes']),
            'Expected exactly four staged adds')
    require(head() == initial_head, 'add published before commit')
    committed = document(a, 'commit', '-m', 'demo: add sample images')
    require(committed == {'head': head(), 'change_count': 4, 'asset_count': 4},
            'Batch commit result does not describe all four Assets')
    require(int(git('rev-list', '--count', head())) == initial_count + 1,
            'The four-Asset batch did not publish exactly one commit')
    require(git('rev-list', '--parents', '-n', '1', head()).split() == [head(), initial_head],
            'Batch commit must have the staging base as its sole parent')
    tree_counts(4, 4, 3)
    verify(a, 4, 4)
    original = assets(a)
    require(set(original) == set(ids), 'Published gallery omitted an Asset')
    for asset_id, (filename, title, tag) in zip(ids, specifications):
        asset = original[asset_id]
        require(asset['title'] == title and asset['tags'] == [tag] and
                asset['collections'] == ['demo-gallery'] and
                asset['namespace'] == 'blog' and asset['original_name'] == filename and
                asset['object']['mime'] == 'image/png' and
                (asset['object']['width'], asset['object']['height']) == (64, 48),
                'Published sample metadata differs from the input')
        placement(asset)
    require(original[red1]['object'] == original[red2]['object'],
            'Repeated red content must keep identical content metadata')
    require(len({asset['object']['hash'] for asset in original.values()}) == 3,
            'The three generated samples must have distinct content hashes')
    red_path = placement(original[red1])
    red2_path = placement(original[red2])
    require(red_path != red2_path, 'Duplicate bytes must not share one placement')
    shared_red_blob = tree_entries()[red_path]
    require(tree_entries()[red2_path] == shared_red_blob,
            'Duplicate bytes must still share one Git blob')
    clean(a)
    print('PASS four Asset adds -> one commit -> four placements sharing three blobs', flush=True)

    document(b, 'repo', 'use', '--local', str(gallery))
    document(b, 'sync')
    require(assets(b) == original, 'Fresh device B did not restore complete metadata')
    require(document(b, 'show', red1)['asset'] == {'asset': original[red1], 'state': 'committed'},
            'show did not restore the full red Asset')
    for query, expected_ids in [('demo', set(ids)), ('demo-red-one', {red1}), ('demo-gallery', set(ids))]:
        matches = document(b, 'find', query)['assets']
        require({view['asset']['id'] for view in matches} == expected_ids,
                f'find {query!r} returned the wrong Assets')
    document(b, 'edit', red1, '--title', 'stale edit')
    document(a, 'rm', red1)
    document(a, 'commit', '-m', 'demo: remove first shared red Asset')
    tree_counts(3, 3, 3)
    current = paths()
    require(red_path not in current and red2_path in current,
            'Removing one Asset must not remove the other red placement')
    require(tree_entries()[red2_path] == shared_red_blob,
            'Surviving Asset lost the shared Git blob')
    after_first_delete = head()
    conflict = document(b, 'commit', '-m', 'demo: stale edit must fail', expected=3)
    require(conflict['error']['code'] == 'conflict', 'Stale edit did not report structured conflict')
    require(head() == after_first_delete and f'.touko/assets/{red1}.json' not in paths(),
            'Stale edit resurrected a deleted Asset or changed HEAD')
    require(len(document(b, 'status')['changes']) == 1, 'Conflict discarded the staged edit')
    document(b, 'reset', '--staged')
    document(b, 'sync')
    require(document(b, 'show', red1, expected=1)['error']['code'] == 'asset_not_found',
            'Deleted Asset remained visible after reset/sync')
    require(set(assets(b)) == {red2, blue, green}, 'Device B did not observe the deletion')
    print('PASS fresh metadata recovery and stale edit cannot resurrect deletion', flush=True)

    document(a, 'rm', red2)
    document(a, 'commit', '-m', 'demo: remove last red reference')
    tree_counts(2, 2, 2)
    current = paths()
    require(red_path not in current and red2_path not in current,
            'Last-reference removal left a current red placement')
    document(c, 'repo', 'use', '--local', str(gallery))
    document(c, 'sync')
    remaining = {asset_id: original[asset_id] for asset_id in [blue, green]}
    require(assets(c) == remaining, 'Fresh device C did not see exactly blue and green')
    verify(c, 2, 2)
    clean(a)
    # Every CLI process above has exited: no live SQLite connection can own WAL/SHM.
    config = (a / 'config.json').read_bytes()
    for suffix in ['', '-wal', '-shm']:
        (a / f'index.db{suffix}').unlink(missing_ok=True)
    require((a / 'config.json').read_bytes() == config, 'Cache removal changed configuration')
    document(a, 'sync')
    require(assets(a) == remaining and document(a, 'status')['head'] == document(c, 'status')['head'] == head(),
            'Rebuilt SQLite cache differs from fresh device C metadata/head')
    verify(a, 2, 2)
    print('PASS last-reference removal and SQLite reconstruction (Git history retained)', flush=True)

    stable_head = head()
    clipboard = cli(a, 'add', '-', '--name', '剪贴板.png', '--namespace', 'notes',
                    '--output', 'id', data=(images / 'red.png').read_bytes()).stdout.decode().strip()
    require(str(uuid.UUID(clipboard)) == clipboard, 'stdin add did not return a UUID')
    pending = document(a, 'status')['changes']
    require(len(pending) == 1 and pending[0]['kind'] == 'add' and
            pending[0]['asset_id'] == clipboard and
            pending[0]['asset']['original_name'] == '剪贴板.png' and
            pending[0]['asset']['namespace'] == 'notes',
            'Piped Unicode stdin metadata did not survive process restart')
    document(a, 'rm', clipboard)
    clean(a)
    require(head() == stable_head and assets(a) == remaining,
            'Canceling a staged stdin add changed the published gallery')

    before = document(a, 'status')
    require(document(a, 'url', blue, expected=1)['error']['code'] == 'delivery_unavailable',
            'Local Git must not pretend to supply a public URL')
    cli(a, 'add', str(images / 'red.png'), '--output', 'markdown', expected=1)
    require(document(a, 'add', str(images / 'red.png'), '--output', 'id', expected=2)['error']['code'] == 'usage',
            'JSON/output-id conflict was not rejected as usage')
    require(document(a, 'status') == before and assets(a) == remaining and head() == stable_head,
            'Unavailable delivery or output-format preflight mutated local/remote state')
    print('PASS persistent piped stdin, canceled add, and local delivery preflight', flush=True)

    before_refusal = filesystem_state()
    run(['bash', project / 'examples' / 'demo.sh', root], expected=1)
    require(filesystem_state() == before_refusal, 'Refusing a nonempty target modified its contents')
    print('PASS rerun refuses the retained nonempty target without modification', flush=True)
    print(f'\nRetained demo directory: {root}\nBinary: {binary}\nGallery: {gallery}')
    print(f'State directories: {a}, {b}, {c}')
    print('Latest HEAD has only blue/green; earlier red bytes remain in Git history.')
    print('Continue from the project root (the logo is only read if you run these commands):')
    for args in [
        [binary, '--state-dir', a, 'ls'],
        [binary, '--state-dir', a, 'add', project / 'logo' / 'TOUKO.png',
         '--namespace', 'blog', '--title', 'My TOUKO logo', '--tag', 'demo'],
        [binary, '--state-dir', a, 'status'],
        [binary, '--state-dir', a, 'commit', '-m', 'demo: add my logo'],
        [binary, '--state-dir', a, 'verify'],
    ]:
        print('  ' + shlex.join([str(part) for part in args]))


try:
    main()
except Exception as error:
    print(f'Demo failed: {error}\nDirectory retained for inspection: {root}', file=sys.stderr)
    sys.exit(1)
PY
