# Publication and recovery

`add/edit/rm/import` only change local staging. `commit -m MESSAGE` publishes one complete batch. Committed Git manifest/Asset files are authoritative; SQLite schema2 is committed cache only, while `workspace.json` durably stores unpublished changes, base and publication journal.

## Commit order

1. Acquire the state directory's nonblocking file lock; reconcile an existing journal first.
2. Plan against the base recorded when staging began. A remote deletion of a locally edited Asset produces `delete_modify`, never resurrection; that is classified when the ref update is rejected, so no HEAD request is needed before uploading.
3. Read the immutable base snapshot and run the pure final-reference planner. Empty plans create no commit.
4. Stream payloads serially, recheck SHA-256/size and verify reused remote placements. Create the tree from the base tree; create a detached commit whose sole parent is staging base.
5. Persist `{BaseHead,CandidateHead,CandidateTree}` as the publication journal in `workspace.json` **before** publishing the ref. The GitHub adapter reads the current HEAD and rejects a mismatch, then requests a non-forced update.
6. On a successful update, record the tree, commit and blob SHAs returned by the accepted writes; commit the cache transaction, then durably replace the workspace to clear changes/journal. Release unreferenced staged payloads only after Save succeeds.

Snapshot metadata reads run with at most eight concurrent requests; validation
and errors remain ordered. During an ordinary commit, unchanged Git entries can
reuse metadata from the freshly validated base. Journal recovery, reset and
sync still re-read and validate the candidate and its descendants; a fresh
commit instead checks its locally constructed state against the accepted plan
(every published Asset must have a matching blob entry) without extra requests.

GitHub commits keep HEAD checks, blob writes, tree/commit creation and the ref
update on the API, but answer reads of immutable commit, tree and blob objects
from the local clone when it already holds them. A missing object falls back to
the API per object, and the clone's own ref is never moved. When a previous API
commit left the clone behind, its indexed commit is fetched once (depth 1,
without FETCH_HEAD) before reading; a failed fetch simply means the API answers
instead. With a locally available base and no extra manifest write, an image
commit needs five API writes plus one pre-publication HEAD read; missing local
objects, recovery or format upgrades can require more. R2 mirroring adds its own
calls, including checks needed to establish ownership and verify the upload.

A published remote commit is not rolled back when local bookkeeping fails. Errors identify the published/candidate HEAD; `sync` reconciles it. Save is deliberately cache-first: failed workspace replacement can leave new cache beside old durable stage/journal, which recovery must retain rather than interpret as a second batch. A directory-sync failure after rename is reported, not represented as guaranteed rollback. Payload-cleanup errors must not be read as remote publication failure.

## Recovery table

| Remote relationship | commit/init | sync |
| --- | --- | --- |
| Candidate or descendant | Validate and finalize, never republish | Validate and finalize |
| Still base | Retry the identical candidate | Clear unpublished journal; retain stage |
| Unrelated | Keep journal/stage; conflict | Keep journal/stage; conflict |
| Relationship read failed | Keep journal/stage; outcome unknown | Keep journal/stage; outcome unknown |

Without a journal, GitHub `reset --staged` discards local changes without HTTP requests; CLI localgit still opens the selected repository, so a missing local repository is not guaranteed to work. With a journal reset first reads authoritative remote relationships: finalize published work, or explicitly discard the confirmed unpublished plan. It never changes remote refs. Ordinary GitHub sync holds the workspace lock, updates the local clone, then reads its fixed HEAD; it is a network operation, and retains staging only if HEAD has not changed. Otherwise explicitly preserve or discard local edits before syncing. Journal recovery must use the GitHub API, not shallow-clone ancestry; successful recovery is followed by clone refresh. `reindex` can rebuild a damaged/deleted schema2 cache while preserving durable stage/journal, but cannot resolve a journal or recover unpublished records from an unreadable unmigrated schema1 database.

## Verification and deletion boundary

`verify` fixes the supplied reader's HEAD, validates complete metadata, then streams every unique referenced placement to check size, SHA-256, format and dimensions. CLI/Web GitHub verification uses the **current clone without fetch**, not necessarily latest remote HEAD. It does not modify remote data. Same Git blob content may be checked once for multiple placements.

`rm` followed by commit removes metadata from the latest HEAD and removes an image only when its final reference to the **entire resolved path** is gone. This includes canonical and imported/custom paths, not merely namespace/hash pairs. Unmanaged files are not garbage-collected. History and CDN caches are **not** purged. No thumbnail subsystem exists.

Local Git uses strict expected-old-SHA `update-ref` CAS. GitHub only supplies non-force fast-forward protection; see the GitHub guide for its external rewrite limitation.

## Optional Cloudflare mirror

GitHub publication remains the success boundary. The only automatic mirror hook runs in `finalizeCandidate`, after candidate/descendant validation, repository-identity checks and successful `Store.Save`, before existing payload cleanup. It uploads only durable Add placements still matching the final snapshot (including explicitly imported images), retries known incomplete work, and reconciles known unreferenced keys. Metadata edits and ordinary clone/cache sync do not backfill the old gallery. Local Git does not auto-mirror.

R2 failure is an optional `cloudflare` result, not a rollback or a second GitHub publication. Original cache/cleanup failures still use their original error paths. Recovery uses the same finalizer; `SyncWith` retains its mirror result through the subsequent clone refresh.

One reconcile engine serves publication and explicit full backfill. Before remote work it durably writes every PUT/delete intent (W1); it processes sorted uploads in batches of at most four, merges their results in order, then saves results once (W2). A crash before W2 can replay W1 by HEAD, adopting matching owned objects without retransmission. Automatic publication can read a matching staged Add payload; missing payloads and full backfill use the fixed Git SHA. Both sources pass through unconverted image ingest and full description validation before PUT.

Creation uses `If-None-Match:*`; replacement of a valid same-repository/source object uses its observed ETag with `If-Match`. Foreign or untagged keys are conflicts, never overwritten. All PUT/adopt work precedes deletion; an unfinished PUT defers every delete. Deletion requires no final Asset reference, a fresh authoritative source HEAD, and confirmed ownership metadata. Deleted tombstones remain known so a late timed-out PUT can be cleaned on a later explicit sync.

No bucket listing or bucket-wide pruning occurs. R2 does not document conditional DELETE: external same-key writes still have a check-to-delete window. This is not a cross-system transaction or a linearizability guarantee. GitHub remains authoritative; reconcile can repair the latest referenced placement.

