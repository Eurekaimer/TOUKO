# Publication and recovery

`add/edit/rm` only change local staging. `commit -m MESSAGE` publishes one complete batch. The committed Git manifest/Asset files are authoritative; SQLite is the local snapshot plus unpublished changes and publication journal.

## Commit order

1. Acquire the state directory's nonblocking file lock; reconcile an existing journal first.
2. Compare remote HEAD with staging base before uploading anything. A remote deletion of a locally edited Asset produces `delete_modify`, never resurrection.
3. Read the immutable base snapshot and run the pure final-reference planner. Empty plans create no commit.
4. Stream payloads serially, recheck SHA-256/size and verify reused remote placements. Create the tree from the base tree; create a detached commit whose sole parent is staging base.
5. Persist `{base_head,candidate_head,candidate_tree}` in SQLite **before** publishing the ref. Check HEAD again; publish using backend ref protection.
6. Validate candidate metadata, placements, deletions and unchanged unrelated leaf entries; confirm remote is candidate or its descendant. Scan the latest valid HEAD and transactionally replace cache, clear changes and journal. Release unreferenced staged payloads only after the database succeeds.

A published remote commit is not rolled back when local bookkeeping fails. Errors identify the published/candidate HEAD; `sync` reconciles it. Payload-cleanup errors must not be read as remote publication failure.

## Recovery table

| Remote relationship | commit/init | sync |
| --- | --- | --- |
| Candidate or descendant | Validate and finalize, never republish | Validate and finalize |
| Still base | Retry the identical candidate | Clear unpublished journal; retain stage |
| Unrelated | Keep journal/stage; conflict | Keep journal/stage; conflict |
| Relationship read failed | Keep journal/stage; outcome unknown | Keep journal/stage; outcome unknown |

`reset --staged` normally works offline and discards all local changes. With an unresolved journal it first reads remote relationships: finalize published work, or explicitly discard the confirmed unpublished plan. It never changes remote refs. Ordinary sync retains staging only if HEAD has not changed; otherwise reset then sync. Do not delete `index.db` while unpublished work exists.

## Verification and deletion boundary

`verify` fixes a remote HEAD, validates complete metadata, then streams every unique referenced placement to check size, SHA-256, format and dimensions. It does not modify remote data. Same Git blob content may be checked once for multiple placements.

`rm` followed by commit removes metadata from the latest HEAD and removes a canonical binary only when its final namespace-local reference is gone. History and CDN caches are **not** purged. No thumbnail subsystem exists.

Local Git uses strict expected-old-SHA `update-ref` CAS. GitHub only supplies non-force fast-forward protection; see the GitHub guide for its external rewrite limitation.
