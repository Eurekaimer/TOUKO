# Repository format v1

A TOUKO repository is a Git tree. Git is the committed source of truth; no database, token, or delivery URL belongs in it. Unknown versions fail closed on read; TOUKO does not migrate or rewrite them.

- `.touko/repository.json`: `{"format_version":1,"repository_id":"<lowercase UUID>","hash_algorithm":"sha256"}` (maximum 64 KiB).
- `.touko/assets/<lowercase asset UUID>.json`: one full Asset per file (maximum 1 MiB), stable JSON plus newline. Fields: `id,namespace,object,original_name,title,tags,collections,created_at,updated_at`. Object fields: `hash,mime,ext,size,width,height`. Timestamps are UTC RFC3339Nano. Empty tags/collections encode as `[]`.
- Objects: `objects/<namespace>/sha256/<first 2 hex>/<next 2 hex>/<64 lowercase SHA-256 hex>.<ext>`.

Asset UUID, content SHA-256, and Git blob SHA-1 are distinct identities. Repeated imports create different Assets. Namespace is a physical route: a single ASCII slug matching `[a-z0-9][a-z0-9_-]{0,62}`, default `default`. Same bytes in one namespace share one placement; across namespaces their content hash is equal but their physical paths differ. Reference counts therefore use the entire canonical object path. PNG/JPEG/GIF extensions (`png/jpg/gif`) come from decoded image format, never the supplied filename.

Asset embeds the complete immutable ContentObject description. Descriptions of the same SHA-256 must agree across all Assets. Metadata ID/filename mismatch, invalid namespaces, duplicate IDs, missing referenced objects, conflicting descriptions, malformed metadata and unsupported versions are corruption; readers must not construct a partial gallery. UTF-8 titles, original filenames, tags and collections are supported. URL delivery is derived, never persisted as metadata.

Selectors accept a full UUID or an unambiguous UUID prefix of at least eight characters. Filenames are not identities. Deletion removes metadata and, only for the last reference in that namespace, the current-tree object. Git history is not purged.
