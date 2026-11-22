# Repository formats v1, v2 and v3

A TOUKO repository is a Git tree. Git is the committed source of truth; no database, token, or delivery URL belongs in it. Readers accept formats 1, 2 and 3; unknown versions fail closed rather than being rewritten.

- `.touko/repository.json`: `{"format_version":1,"repository_id":"<lowercase UUID>","hash_algorithm":"sha256"}` (maximum 64 KiB). Versions 2 and 3 use the same fields; SHA-256 is the algorithm for digests that are present.
- `.touko/assets/<lowercase asset UUID>.json`: one full Asset per file (maximum 1 MiB), stable JSON plus newline. Fields: `id,namespace,object,original_name,title,tags,collections,created_at,updated_at`, plus optional `path` in v2/v3. Object fields: `hash,mime,ext,size,width,height`. Timestamps are UTC RFC3339Nano. Empty tags/collections encode as `[]`.
- Default new placement: `<unix-nanoseconds>[-<collision>].<ext>`, using the time the batch enters staging. An explicitly chosen directory prefixes that filename.

`Asset.Path` is the actual repository-relative image path. Legacy records without it still derive `objects/<namespace>/sha256/aa/bb/<sha256>.<ext>`, so they require a digest. Explicit paths need not have a digest. Paths are normalized relative POSIX without traversal, backslashes, control characters, or `.git`/`.touko` segments. Unicode, spaces and extensionless filenames are supported; placements must be regular Git blobs.

Format v1 forbids nonempty `path`; publishing the first explicit path upgrades to v2. Migration can upgrade v1/v2 to v3, which protects retained R2 keys. Older clients that require SHA-256 cannot read new hashless assets; update them before publishing.

UUID identifies the asset, Git blob ID identifies its committed bytes, and optional SHA-256 enables exact digest verification and duplicate checks. Repeated uploads get separate timestamp paths and may share a Git blob. Namespaces do not relocate explicit paths. The extension of a new upload comes from the detected image format.

An Asset embeds the image description. When SHA-256 is recorded, descriptions with the same digest must agree. Shared placements require a known digest; different hashless Assets must not occupy the same path. Metadata mismatches, missing objects and unsupported versions are corruption.

Selectors accept a full UUID or an unambiguous UUID prefix of at least eight characters. Deletion removes metadata and the current-tree image only on its final reference. It does not purge Git history.

## Importing existing images

`ImportLegacy` scans one fixed initialized HEAD, optionally within a directory, hashes and inspects existing PNG/JPEG/GIF/WebP blobs, and stages metadata referencing their original paths. It does not move, copy, or re-upload image bytes, so existing links remain valid. Already managed paths (including staged additions and canonical placements) are skipped; repeated import is idempotent. Non-images and unsupported entry types count as skipped; validation/read failures reject the batch. An uninitialized repository requires explicit initialization first. Import only stages changes; commit publishes metadata and any required v2 upgrade.

`--directory` keeps a normalized destination prefix, replacing spaces with hyphens. All new owned filenames are timestamps; existing UUID placements and imported paths remain readable.

## Atomic planning

`PlanCommit` applies a normalized batch to a copy of the base Asset map before calculating placements. A stale base, duplicate operation, missing target, or invalid metadata rejects the entire batch. Metadata writes (including a required manifest upgrade) and path deletions are stable-sorted. Deleting one of several references preserves the image; removing the final reference deletes that resolved placement. Delete plus same-path/same-bytes add retains it, regardless of operation order. Equal bytes at different paths remain independent placements.

The plan never performs file/database/network/clock/random IO. It leaves unrelated files and unknown orphan objects untouched. Existing placements are never overwritten; publication verifies reused bytes. Empty or metadata-identical plans create no commit.
