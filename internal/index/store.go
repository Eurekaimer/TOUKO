package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/Eurekaimer/TOUKO/internal/domain"
	"github.com/Eurekaimer/TOUKO/internal/repository"
	_ "modernc.org/sqlite"
)

type Publication struct {
	BaseHead      domain.RepositoryHead
	CandidateHead domain.RepositoryHead
	CandidateTree string
}

type State struct {
	Head        domain.RepositoryHead
	Tree        string
	Manifest    *domain.Repository
	BaseHead    domain.RepositoryHead
	Assets      map[domain.AssetID]domain.Asset
	Changes     map[domain.AssetID]domain.Change
	Publication *Publication
}

type Store struct {
	DB *sql.DB
}

const schema = `
CREATE TABLE repository_state (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    head TEXT NOT NULL,
    tree TEXT NOT NULL,
    manifest BLOB,
    staging_base_head TEXT NOT NULL
);
INSERT INTO repository_state (singleton, head, tree, manifest, staging_base_head) VALUES (1, '', '', NULL, '');
CREATE TABLE assets (id TEXT PRIMARY KEY, metadata BLOB NOT NULL);
CREATE TABLE changes (asset_id TEXT PRIMARY KEY, kind TEXT NOT NULL, metadata BLOB);
CREATE TABLE publication (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    base_head TEXT NOT NULL,
    candidate_head TEXT NOT NULL,
    candidate_tree TEXT NOT NULL
);
PRAGMA user_version = 1;
`

func Open(ctx context.Context, stateDir string) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	if err := os.Chmod(stateDir, 0700); err != nil {
		return nil, fmt.Errorf("secure state directory: %w", err)
	}
	path, err := filepath.Abs(filepath.Join(stateDir, "index.db"))
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("create index: %w", err)
	}
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return nil, fmt.Errorf("secure index: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{DB: db}
	if err := s.initialize(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open index: %w", err)
	}
	return s, nil
}

func (s *Store) initialize(ctx context.Context) error {
	var version int
	if err := s.DB.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != 0 && version != 1 {
		return corrupt("unsupported SQLite user_version %d", version)
	}
	for _, pragma := range []string{"PRAGMA journal_mode = WAL", "PRAGMA foreign_keys = ON"} {
		if _, err := s.DB.ExecContext(ctx, pragma); err != nil {
			return err
		}
	}
	if version == 1 {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Close() error {
	return s.DB.Close()
}

func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: %s", domain.ErrRepositoryCorrupt, fmt.Sprintf(format, args...))
}

func (s *Store) Load(ctx context.Context) (State, error) {
	state := State{Assets: make(map[domain.AssetID]domain.Asset), Changes: make(map[domain.AssetID]domain.Change)}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return State{}, err
	}
	defer tx.Rollback()
	var manifest []byte
	err = tx.QueryRowContext(ctx, "SELECT head, tree, manifest, staging_base_head FROM repository_state WHERE singleton = 1").Scan(&state.Head, &state.Tree, &manifest, &state.BaseHead)
	if errors.Is(err, sql.ErrNoRows) {
		return State{}, corrupt("missing repository state")
	}
	if err != nil {
		return State{}, err
	}
	if manifest != nil {
		m, err := repository.DecodeManifest(manifest)
		if err != nil {
			return State{}, err
		}
		state.Manifest = &m
	}
	if err := loadAssets(ctx, tx, state.Assets); err != nil {
		return State{}, err
	}
	if err := loadChanges(ctx, tx, state.Changes); err != nil {
		return State{}, err
	}
	var publication Publication
	err = tx.QueryRowContext(ctx, "SELECT base_head, candidate_head, candidate_tree FROM publication WHERE singleton = 1").Scan(&publication.BaseHead, &publication.CandidateHead, &publication.CandidateTree)
	if err == nil {
		state.Publication = &publication
	} else if !errors.Is(err, sql.ErrNoRows) {
		return State{}, err
	}
	if err := tx.Commit(); err != nil {
		return State{}, err
	}
	return state, nil
}

func loadAssets(ctx context.Context, tx *sql.Tx, assets map[domain.AssetID]domain.Asset) error {
	rows, err := tx.QueryContext(ctx, "SELECT id, metadata FROM assets")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var metadata []byte
		if err := rows.Scan(&id, &metadata); err != nil {
			return err
		}
		a, err := repository.DecodeAsset(metadata, id+".json")
		if err != nil {
			return err
		}
		assets[a.ID] = a
	}
	return rows.Err()
}

func loadChanges(ctx context.Context, tx *sql.Tx, changes map[domain.AssetID]domain.Change) error {
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

func (s *Store) Save(ctx context.Context, state State) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var manifest []byte
	if state.Manifest != nil {
		manifest, err = repository.EncodeManifest(*state.Manifest)
		if err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM assets"); err != nil {
		return err
	}
	for id, a := range state.Assets {
		if id != a.ID {
			return corrupt("asset map key differs from metadata ID")
		}
		metadata, err := repository.EncodeAsset(a)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO assets (id, metadata) VALUES (?, ?)", id.String(), metadata); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM changes"); err != nil {
		return err
	}
	for id, change := range state.Changes {
		if id == (domain.AssetID{}) || id != change.AssetID {
			return corrupt("change map key differs from asset ID or is empty")
		}
		var metadata []byte
		switch change.Kind {
		case domain.Add, domain.Update:
			if change.Asset == nil || change.Asset.ID != id {
				return corrupt("change %s has missing or mismatched metadata", id)
			}
			metadata, err = repository.EncodeAsset(*change.Asset)
			if err != nil {
				return err
			}
		case domain.Delete:
			if change.Asset != nil {
				return corrupt("delete change %s contains metadata", id)
			}
		default:
			return corrupt("invalid change kind %q", change.Kind)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO changes (asset_id, kind, metadata) VALUES (?, ?, ?)", id.String(), string(change.Kind), metadata); err != nil {
			return err
		}
	}
	if err := savePublication(ctx, tx, state.Publication); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "UPDATE repository_state SET head = ?, tree = ?, manifest = ?, staging_base_head = ? WHERE singleton = 1", string(state.Head), state.Tree, manifest, string(state.BaseHead))
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return corrupt("missing repository state")
	}
	return tx.Commit()
}

func (s *Store) SavePublication(ctx context.Context, publication *Publication) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := savePublication(ctx, tx, publication); err != nil {
		return err
	}
	return tx.Commit()
}

func savePublication(ctx context.Context, tx *sql.Tx, publication *Publication) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM publication"); err != nil {
		return err
	}
	if publication != nil {
		_, err := tx.ExecContext(ctx, "INSERT INTO publication (singleton, base_head, candidate_head, candidate_tree) VALUES (1, ?, ?, ?)", string(publication.BaseHead), string(publication.CandidateHead), publication.CandidateTree)
		return err
	}
	return nil
}
