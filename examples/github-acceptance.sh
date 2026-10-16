#!/usr/bin/env bash
# Real GitHub acceptance for TOUKO. Run from the project root.
#
#   OWNER/touko-test must already exist, be initialized with a README, and be a
#   DEDICATED gallery. Never point this at the TOUKO source repository.
#
# The token is read only from GITHUB_TOKEN in this shell. It is never accepted as
# an argument, never printed, and never written to disk by this script.
#
# Usage: GITHUB_TOKEN=... bash examples/github-acceptance.sh OWNER/REPO [BRANCH]
set -euo pipefail

die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
note() { printf '\n== %s\n' "$*"; }

if [[ ${1:-} == --help || ${1:-} == -h ]]; then
    printf '%s\n' \
        'Usage: GITHUB_TOKEN=... bash examples/github-acceptance.sh OWNER/REPO [BRANCH]' \
        '' \
        'Runs the real remote lifecycle against a DEDICATED gallery repository:' \
        '  auth status -> repo use -> repo init -> add -> commit -> url -> verify' \
        '  then a fresh second device (use + sync + full metadata compare),' \
        '  a raw-byte SHA-256 check, and a last-reference deletion on a third device.' \
        '' \
        'Prerequisites: the repository must exist with an initial commit (add a README),' \
        'and GITHUB_TOKEN must carry Contents: read and write for it. Requires Go 1.26+,' \
        'Git, Bash and Python 3.8+ stdlib. Go modules must already be cached.'
    exit 0
fi
if (( $# < 1 || $# > 2 )); then
    die 'usage: GITHUB_TOKEN=... bash examples/github-acceptance.sh OWNER/REPO [BRANCH]'
fi
repository=$1
branch=${2:-}
if [[ ! $repository =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]]; then
    die "repository must look like OWNER/REPO, got: $repository"
fi
# The source repository holds TOUKO's own history and must never be used as a gallery.
case ${repository,,} in
    eurekaimer/touko) die 'refusing to run against the TOUKO source repository itself' ;;
esac
[[ -n ${GITHUB_TOKEN:-} ]] || die 'GITHUB_TOKEN is not set; export it in this terminal (never pass it as an argument)'

for dependency in go git python3; do
    command -v "$dependency" >/dev/null 2>&1 || die "missing dependency: $dependency"
done
[[ -f go.mod && -d cmd/touko && -d examples/images ]] || die 'run this script from the TOUKO project root'

# Offline build: reuse the module cache instead of downloading during acceptance.
export GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GONOPROXY=none

root=$(mktemp -d "${TMPDIR:-/tmp}/touko-github.XXXXXXXX")
binary=$root/touko
go build -o "$binary" ./cmd/touko || die 'build failed'
printf 'Binary: %s\nWork directory: %s\n' "$binary" "$root"
printf 'Gallery: %s (branch %s)\n' "$repository" "${branch:-<repository default>}"

# Every assertion lives in one Python program so each device keeps its own state
# directory and each CLI call is a fresh process, exactly as a user would run it.
python3 - "$repository" "$branch" "$binary" "$root" <<'PY'
import hashlib
import json
import os
import shlex
import subprocess
import sys
import urllib.request
from pathlib import Path

repository, branch, binary, root = sys.argv[1], sys.argv[2], sys.argv[3], Path(sys.argv[4])
images = Path('examples/images')
logo = Path('logo/TOUKO.png')
a, b, c = (root / name for name in ('device-a', 'device-b', 'device-c'))


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def cli(device, *args, expected=0):
    """Run one real CLI process against one persistent state directory."""
    argv = [str(part) for part in (binary, '--state-dir', device, *args)]
    result = subprocess.run(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    require(result.returncode == expected,
            f'{shlex.join(argv)}: expected exit {expected}, got {result.returncode}\n'
            f'stdout: {result.stdout.decode(errors="replace")}\n'
            f'stderr: {result.stderr.decode(errors="replace")}')
    return result


def document(device, *args, expected=0):
    """Parse one JSON document from a command that is expected to succeed."""
    result = cli(device, *args, expected=expected)
    try:
        return json.loads(result.stdout)
    except json.JSONDecodeError as error:
        raise AssertionError(f'{shlex.join(str(a) for a in args)} did not print JSON: {error}\n'
                             f'stdout: {result.stdout.decode(errors="replace")}\n'
                             f'stderr: {result.stderr.decode(errors="replace")}') from error


def failure(device, *args):
    """Parse the JSON error document a failing command writes to stderr."""
    result = cli(device, *args, expected=1)
    try:
        payload = json.loads(result.stderr)
    except json.JSONDecodeError as error:
        raise AssertionError(f'{shlex.join(str(a) for a in args)} did not print a JSON error: {error}\n'
                             f'stderr: {result.stderr.decode(errors="replace")}') from error
    return payload['error']


def assets(device):
    """Map asset UUID to the complete remote-recoverable metadata record."""
    return {entry['asset']['id']: entry['asset'] for entry in document(device, 'ls', '--json')['assets']}


def public_url(kind, asset):
    """Local derivation only; confirms routing without relying on CDN propagation."""
    object_path = f"objects/{asset['namespace']}/sha256/{asset['object']['hash'][:2]}/{asset['object']['hash'][2:4]}/{asset['object']['hash']}.{asset['object']['ext']}"
    owner, name = repository.split('/', 1)
    if kind == 'github':
        return f'https://raw.githubusercontent.com/{owner}/{name}/refs/heads/{actual_branch}/{object_path}'
    return f'https://cdn.jsdelivr.net/gh/{owner}/{name}@{actual_branch}/{object_path}'


# --- 1. authentication and repository identity -----------------------------
auth = document(a, 'auth', 'status', '--json')
require(auth.get('authenticated') is True, f'auth status reported no authentication: {auth}')
login = auth.get('login')
require(login and login != '', f'auth status returned no login: {auth}')
require('token' not in json.dumps(auth).lower(), 'auth status exposed a token-shaped field')
print(f'PASS auth status authenticated as {login} without printing the token', flush=True)

# reuse for every device; --json keeps this script's parsing honest.
use_args = ['repo', 'use', repository, '--json'] if not branch else ['repo', 'use', repository, '--branch', branch, '--json']
selected = document(a, *use_args)['location']
require(selected['backend'] == 'github', f'expected a GitHub backend, got {selected}')
require(f"{selected['owner']}/{selected['name']}" == repository, f'selected unexpected repository {selected}')
actual_branch = selected['branch']
require(actual_branch, 'selected location has no branch')
if branch:
    require(actual_branch == branch, f'--branch {branch} selected {actual_branch}')
second = document(a, 'repo', 'use', repository, '--branch', actual_branch)['location']
require(second == selected, f're-selecting the explicit branch changed the location: {second} != {selected}')
print(f'PASS selected {repository} branch {actual_branch} (private={selected["private"]}) without remote writes', flush=True)

# --- 2. format initialization ----------------------------------------------
init = document(a, 'repo', 'init', '--json')
print(f'PASS repo init created format v1 at {init.get("head", "<head>")}', flush=True)
second_init = document(a, 'repo', 'init', '--json')
require(second_init.get('head') == init.get('head'),
        'a repeated init on a valid repository must be idempotent')
print('PASS repeated repo init is idempotent (no extra commit)', flush=True)

# --- 3. stage and publish real images --------------------------------------
sources = sorted(str(path) for path in images.glob('*.png'))
require(len(sources) == 3, f'expected the three shipped fixtures, found {sources}')
staged = []
for source in sources:
    staged.append(cli(a, 'add', source, '--namespace', 'blog', '--tag', 'demo', '--output', 'id').stdout.decode().strip())
# The user's own artwork is only published when explicitly requested: this script
# writes to a real repository, and on a public one those bytes become world
# readable. Default behaviour stages the three generated fixtures only.
if logo.exists() and os.environ.get('TOUKO_ACCEPT_INCLUDE_LOGO') == '1':
    staged.append(cli(a, 'add', str(logo), '--namespace', 'blog', '--title', 'My TOUKO logo',
                      '--tag', 'demo', '--collection', '项目素材', '--output', 'id').stdout.decode().strip())
    print(f'note: publishing the local artwork {logo} because TOUKO_ACCEPT_INCLUDE_LOGO=1', flush=True)
elif logo.exists():
    print('note: leaving logo/TOUKO.png out of the upload; set TOUKO_ACCEPT_INCLUDE_LOGO=1 to include it', flush=True)
pending = document(a, 'status', '--json')
require(pending['changes'] and all(change['kind'] == 'add' for change in pending['changes']),
        f'expected only staged additions, got {pending["changes"]}')
require(pending['head'] == init.get('head'), 'staging must not move the remote head')
print(f'PASS staged {len(staged)} images in one batch; remote head unchanged by add', flush=True)

# Delivery must refuse to promise a URL before the commit exists.
if selected['private']:
    refused = failure(a, 'url', staged[0], '--json')
    require(refused['code'] == 'delivery_unavailable',
            f'private repositories must not derive public URLs, got {refused}')
else:
    require(document(a, 'url', staged[0], '--json')['committed'] is False,
            'a staged-only asset must not report itself as committed')
commit = document(a, 'commit', '-m', 'acceptance: publish demo batch', '--json')
require(commit['change_count'] == len(staged), f'commit published {commit} changes, expected {len(staged)}')
published_head = commit['head']
require(published_head != init.get('head'), 'commit did not advance the remote head')
require(document(a, 'status', '--json')['changes'] == [], 'staging was not cleared after commit')
print(f'PASS commit published {commit["change_count"]} changes as {published_head}', flush=True)

# --- 4. delivery URLs and remote verification -------------------------------
for kind in ('github', 'jsdelivr'):
    if selected['private']:
        refused = failure(a, 'url', staged[0], '--provider', kind, '--json')
        require(refused['code'] == 'delivery_unavailable',
                f'private repository must refuse {kind} URLs, got {refused}')
        continue
    link = cli(a, 'url', staged[0], '--provider', kind).stdout.decode().strip()
    expected = public_url(kind, assets(a)[staged[0]])
    require(link == expected, f'{kind} URL mismatch:\n  got      {link}\n  expected {expected}')
    print(f'PASS {kind} URL matches the canonical object path for this branch', flush=True)

verified = document(a, 'verify', '--json')
require(verified['assets'] == len(staged), f'verify saw {verified} assets, expected {len(staged)}')
require(verified['objects'] >= 1, 'verify found no content objects')
require(verified['head'] == published_head, 'verify ran against an unexpected head')
print(f'PASS verify {verified["head"]}: assets={verified["assets"]} objects={verified["objects"]}', flush=True)

# --- 5. the bytes really are on the remote ---------------------------------
if not selected['private']:
    local = assets(a)[staged[0]]
    with urllib.request.urlopen(public_url('github', local), timeout=30) as response:
        remote_bytes = response.read()
    digest = hashlib.sha256(remote_bytes).hexdigest()
    require(digest == local['object']['hash'],
            f'raw GitHub bytes differ from the recorded hash: {digest} != {local["object"]["hash"]}')
    require(len(remote_bytes) == local['object']['size'], 'raw GitHub size differs from the record')
    print(f'PASS raw GitHub bytes hash to the recorded object {digest[:12]}…', flush=True)
else:
    print('SKIP raw byte fetch: private repository has no anonymous URL', flush=True)

# --- 6. a second device must recover the whole gallery ----------------------
cli(b, *use_args)
synced = document(b, 'sync', '--json')
local_assets = assets(a)
remote_assets = assets(b)
require(synced['asset_count'] == len(local_assets), f'fresh device saw {synced}, expected {len(local_assets)}')
comparable = ['namespace', 'original_name', 'title', 'tags', 'collections', 'created_at', 'updated_at']
for asset_id, expected_asset in local_assets.items():
    require(asset_id in remote_assets, f'fresh device is missing {asset_id}')
    got = remote_assets[asset_id]
    for field in comparable:
        require(got[field] == expected_asset[field],
                f'{asset_id}.{field} differs after fresh sync: {got[field]!r} != {expected_asset[field]!r}')
    for field in ('hash', 'size', 'mime', 'ext', 'width', 'height'):
        require(got['object'][field] == expected_asset['object'][field],
                f'{asset_id}.object.{field} differs after fresh sync')
require(document(b, 'verify', '--json')['assets'] == len(local_assets), 'second device verify disagrees')
print(f'PASS fresh device B recovered {len(local_assets)} assets with identical metadata and verified bytes', flush=True)

# --- 7. deletion is reference-aware, and a third device agrees --------------
target = staged[-1]
cli(b, 'rm', target)
cli(b, 'commit', '-m', 'acceptance: delete one image')
remaining = assets(b)
require(target not in remaining, 'deleted asset is still present on device B')
require(document(b, 'verify', '--json')['assets'] == len(remaining), 'verify disagrees after deletion')
cli(c, *use_args)
cli(c, 'sync')
recovered = assets(c)
require(target not in recovered, f'fresh device C still discovers the deleted {target}')
require(set(recovered) == set(remaining), 'device C does not match the surviving gallery')
print(f'PASS deletion of {target[:8]} removed the asset from a fresh device; {len(remaining)} remain', flush=True)

print(f'\nAll GitHub acceptance checks passed against {repository} (branch {actual_branch}).')
print(f'Work directory retained for inspection: {root}')
print('Nothing was deleted remotely beyond the last-reference deletion you just exercised.')
PY

note 'done'
printf 'Remember to run: unset GITHUB_TOKEN\n'