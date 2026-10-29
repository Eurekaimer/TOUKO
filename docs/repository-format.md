# Repository formats v1 and v2

A TOUKO repository is a Git tree. Git is the committed source of truth; no database, token, or delivery URL belongs in it. Readers accept format 1 and 2; unknown versions fail closed rather than being rewritten.

- `.touko/repository.json`: `{"format_version":1,"repository_id":"<lowercase UUID>","hash_algorithm":"sha256"}` (maximum 64 KiB). Version 2 uses the same fields with `format_version:2`.
- `.touko/assets/<lowercase asset UUID>.json`: one full Asset per file (maximum 1 MiB), stable JSON plus newline. Fields: `id,namespace,object,original_name,title,tags,collections,created_at,updated_at`, plus optional `path` in v2. Object fields: `hash,mime,ext,size,width,height`. Timestamps are UTC RFC3339Nano. Empty tags/collections encode as `[]`.
- Default placement: `objects/<namespace>/<asset-uuid>.<ext>`. The Asset ID names the file, so a path never depends on hashing. Repositories written before this layout keep `objects/<namespace>/sha256/<first 2 hex>/<next 2 hex>/<64 lowercase SHA-256 hex>.<ext>` for Assets without an explicit path, and stay readable.

`Asset.Path` is the actual repository-relative image path when nonempty. `repository.ObjectPath(asset)` returns it, otherwise derives the legacy content-addressed placement with `LegacyObjectPath(namespace, object)`. New uploads always record `AssetObjectPath(namespace, id, ext)`, so they need no hash and every asset owns one object: uploading the same bytes twice creates two assets, two paths, and two tree entries (Git still stores one blob). A path must be normalized relative POSIX, without traversal, backslashes, control characters, or case-insensitive `.git`/`.touko` components. Unicode, spaces, and extensionless filenames are supported. Referenced images must be regular Git blobs (`100644` or `100755`), never symlinks.

Format v1 forbids nonempty `path`. New repositories initialize as v1; ordinary canonical uploads and metadata edits do not upgrade them. The first published batch whose final Assets contain an explicit path atomically writes a v2 manifest with its metadata. V2 still permits Assets without `path`, and does not downgrade when the last explicit path is removed. Old v1-only clients reject the new manifest instead of misreading image locations.

Asset UUID, content SHA-256, and Git blob SHA-1 are distinct identities. The UUID is identity and path; SHA-256 remains integrity metadata only (verify, R2 receipt, legacy paths). Repeated uploads create distinct Assets and distinct objects. Namespace is a single ASCII slug matching `[a-z0-9][a-z0-9_-]{0,62}`, default `default`. It determines canonical placement, but does not relocate an explicit path. Same bytes share a Git blob, but a placement is owned by one asset; canonical and explicit paths both participate in reference counts for repositories that still hold them. PNG/JPEG/GIF/WebP object extensions (`png/jpg/gif/webp`) come from decoded content, never the supplied filename; an explicit path need not have that extension.

Asset embeds the complete immutable ContentObject description. Descriptions of the same SHA-256 must agree across all Assets, as must descriptions sharing one path. Metadata ID/filename mismatch, invalid namespaces/paths, duplicate IDs, missing referenced objects, conflicting descriptions, malformed metadata and unsupported versions are corruption; readers must not construct a partial gallery. UTF-8 titles, original filenames, tags and collections are supported. URL delivery is derived from the actual path, never persisted.

Selectors accept a full UUID or an unambiguous UUID prefix of at least eight characters. Filenames are not identities. Deletion removes metadata and, only for the final reference to the full resolved path, the current-tree image. With asset-id paths that is always the deleted asset's own object. Git history is not purged.

## Importing existing images

`ImportLegacy` scans one fixed initialized HEAD, optionally within a directory, hashes and inspects existing PNG/JPEG/GIF/WebP blobs, and stages metadata referencing their original paths. It does not move, copy, or re-upload image bytes, so existing links remain valid. Already managed paths (including staged additions and canonical placements) are skipped; repeated import is idempotent. Non-images and unsupported entry types count as skipped; validation/read failures reject the batch. An uninitialized repository requires explicit initialization first. Import only stages changes; commit publishes metadata and any required v2 upgrade.

New uploads may also specify a directory: placement becomes `<directory>/<asset-uuid>.<ext>` via explicit `path`, with spaces in each directory segment turned into hyphens so the URL needs no escaping. Default uploads use `objects/<namespace>/<asset-uuid>.<ext>`. Because every new asset has an explicit path, the first such commit upgrades the manifest to v2 through the existing atomic upgrade.

## Atomic planning

`PlanCommit` applies a normalized batch to a copy of the base Asset map before calculating placements. A stale base, duplicate operation, missing target, or invalid metadata rejects the entire batch. Metadata writes (including a required manifest upgrade) and path deletions are stable-sorted. Deleting one of several references preserves the image; removing the final reference deletes that resolved placement. Delete plus same-path/same-bytes add retains it, regardless of operation order. Equal bytes at different paths remain independent placements.

The plan never performs file/database/network/clock/random IO. It leaves unrelated files and unknown orphan objects untouched. Existing placements are never overwritten; publication verifies reused bytes. Empty or metadata-identical plans create no commit.
