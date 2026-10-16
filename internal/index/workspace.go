package index

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/repository"
)

const workspaceName = "workspace.json"

type workspace struct {
	Version      int                              `json:"version"`
	RepositoryID string                           `json:"repository_id,omitempty"`
	BaseHead     domain.RepositoryHead            `json:"base_head"`
	Changes      map[domain.AssetID]domain.Change `json:"changes"`
	Publication  *Publication                     `json:"publication,omitempty"`
}

func validateChanges(changes map[domain.AssetID]domain.Change) error {
	for id, change := range changes {
		if id == (domain.AssetID{}) || id != change.AssetID {
			return corrupt("change map key differs from asset ID or is empty")
		}
		switch change.Kind {
		case domain.Add, domain.Update:
			if change.Asset == nil || change.Asset.ID != id {
				return corrupt("change %s has missing or mismatched metadata", id)
			}
			if _, err := repository.EncodeAsset(*change.Asset); err != nil {
				return err
			}
		case domain.Delete:
			if change.Asset != nil {
				return corrupt("delete change %s contains metadata", id)
			}
		default:
			return corrupt("invalid change kind %q", change.Kind)
		}
	}
	return nil
}

func validateWorkspace(state State) error {
	if err := validateChanges(state.Changes); err != nil {
		return err
	}
	if p := state.Publication; p != nil && (p.CandidateHead == "" || p.CandidateTree == "") {
		return corrupt("incomplete publication journal")
	}
	if state.Manifest != nil && state.Manifest.ID == (domain.RepositoryID{}) {
		return corrupt("invalid workspace repository identity")
	}
	return nil
}

func encodeWorkspace(state State) ([]byte, error) {
	if err := validateWorkspace(state); err != nil {
		return nil, err
	}
	w := workspace{Version: 1, BaseHead: state.BaseHead, Changes: state.Changes, Publication: state.Publication}
	if state.Manifest != nil {
		w.RepositoryID = state.Manifest.ID.String()
	}
	return json.Marshal(w)
}

// LoadWorkspace reads unpublished work without opening the disposable cache.
// Missing or damaged durable state is an error, never an empty staged batch.
// Manifest, when present, contains only the repository identity for recovery.
func LoadWorkspace(ctx context.Context, dir string) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, workspaceName))
	if err != nil {
		return State{}, fmt.Errorf("read durable workspace (open a healthy schema1 index to migrate first): %w", err)
	}
	var w workspace
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&w); err != nil {
		return State{}, corrupt("invalid durable workspace: %v", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return State{}, corrupt("trailing data in durable workspace")
	}
	if w.Version != 1 {
		return State{}, corrupt("unsupported durable workspace version %d", w.Version)
	}
	state := State{BaseHead: w.BaseHead, Changes: w.Changes, Publication: w.Publication, Assets: make(map[domain.AssetID]domain.Asset)}
	if state.Changes == nil {
		state.Changes = make(map[domain.AssetID]domain.Change)
	}
	if w.RepositoryID != "" {
		id, err := domain.ParseRepositoryID(w.RepositoryID)
		if err != nil {
			return State{}, corrupt("invalid workspace repository identity: %v", err)
		}
		state.Manifest = &domain.Repository{ID: id}
	}
	if err := validateWorkspace(state); err != nil {
		return State{}, err
	}
	return state, nil
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func saveWorkspace(ctx context.Context, dir string, state State) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := encodeWorkspace(state)
	if err != nil {
		return err
	}
	return writeWorkspace(ctx, dir, data)
}

func writeWorkspace(ctx context.Context, dir string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".workspace-*")
	if err != nil {
		return fmt.Errorf("create durable workspace: %w", err)
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(append(data, '\n')); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(name, filepath.Join(dir, workspaceName)); err != nil {
		return fmt.Errorf("replace durable workspace: %w", err)
	}
	return syncDirectory(dir)
}

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	state := State{Changes: make(map[domain.AssetID]domain.Change)}
	var manifest []byte
	if err := tx.QueryRowContext(ctx, "SELECT staging_base_head, manifest FROM repository_state WHERE singleton = 1").Scan(&state.BaseHead, &manifest); err != nil {
		return err
	}
	// Committed metadata is disposable; a damaged manifest must not prevent
	// rescuing valid staged changes from the legacy database.
	if manifest != nil {
		if m, err := repository.DecodeManifest(manifest); err == nil {
			state.Manifest = &m
		}
	}
	if err := loadLegacyChanges(ctx, tx, state.Changes); err != nil {
		return err
	}
	var p Publication
	err = tx.QueryRowContext(ctx, "SELECT base_head, candidate_head, candidate_tree FROM publication WHERE singleton = 1").Scan(&p.BaseHead, &p.CandidateHead, &p.CandidateTree)
	if err == nil {
		state.Publication = &p
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	data, err := encodeWorkspace(state)
	if err != nil {
		return err
	}
	if existing, err := LoadWorkspace(ctx, s.dir); err == nil {
		previous, err := encodeWorkspace(existing)
		if err != nil {
			return err
		}
		if !bytes.Equal(previous, data) {
			return corrupt("legacy index and durable workspace disagree; preserve both for recovery")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else if err := saveWorkspace(ctx, s.dir, state); err != nil {
		return err
	}
	// Only drop the legacy source after its complete durable copy has reached disk.
	if _, err := tx.ExecContext(ctx, `DROP TABLE changes; DROP TABLE publication;
ALTER TABLE repository_state DROP COLUMN staging_base_head;
PRAGMA user_version = 2;`); err != nil {
		return err
	}
	return tx.Commit()
}

func loadLegacyChanges(ctx context.Context, tx *sql.Tx, changes map[domain.AssetID]domain.Change) error {
	rows, err := tx.QueryContext(ctx, "SELECT asset_id, kind, metadata FROM changes")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var textID string
		var kind domain.ChangeKind
		var metadata []byte
		if err := rows.Scan(&textID, &kind, &metadata); err != nil {
			return err
		}
		id, err := domain.ParseAssetID(textID)
		if err != nil {
			return corrupt("invalid staged asset ID: %v", err)
		}
		change := domain.Change{Kind: kind, AssetID: id}
		switch kind {
		case domain.Add, domain.Update:
			a, err := repository.DecodeAsset(metadata, textID+".json")
			if err != nil {
				return err
			}
			change.Asset = &a
		case domain.Delete:
			if metadata != nil {
				return corrupt("delete change %s contains metadata", textID)
			}
		default:
			return corrupt("invalid change kind %q", kind)
		}
		changes[id] = change
	}
	return rows.Err()
}
