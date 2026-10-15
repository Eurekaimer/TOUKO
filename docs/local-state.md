# Local state

State directory precedence: `--state-dir`, `TOUKO_HOME`, `$XDG_DATA_HOME/touko`, then `$HOME/.local/share/touko`. Directory permissions are 0700; database, config and staged files are 0600. A stable `.lock` uses nonblocking OS file locking; a busy operation fails instead of deleting a supposedly stale lock.

`config.json` selects a repository (`backend,branch,path,owner,name,private`). It never stores credentials. The SQLite `index.db` uses schema/user_version 1, WAL, foreign keys and one connection. Its committed snapshot is a rebuildable cache; **changes and publication journal are unpublished state**. Removing the database loses unpublished work. With empty staging, keeping config and running sync can reconstruct the committed gallery.

Ingest streams a source once into a synced temporary file while computing SHA-256 and size (maximum 100,000,000 bytes); dimensions and PNG/JPEG/GIF format come from that file's header. Staged placements are `stage/objects/<namespace>/<hash>.<ext>`. Existing placements are checked rather than overwritten. A failed batch leaves no partial staged records and releases only payloads without other staged references.

First change binds staging to the committed HEAD. Add/edit stays add; add/remove cancels; committed edits merge; remove after edit becomes delete. Repeated deletion is harmless; edit after staged deletion conflicts. Selectors resolve against committed IDs plus staged adds before deletion state is checked. Multi-selector removal is atomic. Tags and collections are trimmed, sorted, deduplicated and case-preserving. Read views overlay staging; they hide deletions and label records committed/added/modified.

`reset --staged` discards all unpublished local changes; it does not change remote history. An unresolved publication must be reconciled with the remote before discarding its journal.
