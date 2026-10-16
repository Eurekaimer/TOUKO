package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"

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
	DB  *sql.DB
	dir string
	mu  sync.Mutex
}

const schema = `
CREATE TABLE repository_state (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    head TEXT NOT NULL,
    tree TEXT NOT NULL,
    manifest BLOB
);
INSERT INTO repository_state (singleton, head, tree, manifest) VALUES (1, '', '', NULL);
CREATE TABLE assets (id TEXT PRIMARY KEY, metadata BLOB NOT NULL);
PRAGMA user_version = 2;
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
	s := &Store{DB: db, dir: filepath.Dir(path)}
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
	if version < 0 || version > 2 {
		return corrupt("unsupported SQLite user_version %d", version)
	}
	for _, pragma := range []string{"PRAGMA journal_mode = WAL", "PRAGMA foreign_keys = ON"} {
		if _, err := s.DB.ExecContext(ctx, pragma); err != nil {
			return err
		}
	}
	if version == 1 {
		return s.migrate(ctx)
	}
	if version == 2 {
		_, err := LoadWorkspace(ctx, s.dir)
		return err
	}
	if _, err := os.Stat(filepath.Join(s.dir, workspaceName)); errors.Is(err, os.ErrNotExist) {
		if err := saveWorkspace(ctx, s.dir, State{}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if _, err := LoadWorkspace(ctx, s.dir); err != nil {
		return err
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

func (s *Store) Close() error { return s.DB.Close() }

func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: %s", domain.ErrRepositoryCorrupt, fmt.Sprintf(format, args...))
}

func (s *Store) Load(ctx context.Context) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := LoadWorkspace(ctx, s.dir)
	if err != nil {
		return State{}, err
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return State{}, err
	}
	defer tx.Rollback()
	var manifest []byte
	err = tx.QueryRowContext(ctx, "SELECT head, tree, manifest FROM repository_state WHERE singleton = 1").Scan(&state.Head, &state.Tree, &manifest)
	if errors.Is(err, sql.ErrNoRows) {
		return State{}, corrupt("missing repository state")
	}
	if err != nil {
		return State{}, err
	}
	identity := state.Manifest
	state.Manifest = nil
	if manifest != nil {
		m, err := repository.DecodeManifest(manifest)
		if err != nil {
			return State{}, err
		}
		state.Manifest = &m
		if identity != nil && identity.ID != m.ID {
			return State{}, corrupt("cache repository identity differs from durable workspace")
		}
	}
	if err := loadAssets(ctx, tx, state.Assets); err != nil {
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

func (s *Store) Save(ctx context.Context, state State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Validate both halves before changing either. The committed cache is written
	// first: a failed final workspace rename must leave the publication journal
	// and its staged batch intact, even if the new committed HEAD is already cached.
	workspaceData, err := encodeWorkspace(state)
	if err != nil {
		return err
	}
	var manifest []byte
	if state.Manifest != nil {
		manifest, err = repository.EncodeManifest(*state.Manifest)
		if err != nil {
			return err
		}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO repository_state (singleton, head, tree, manifest) VALUES (1, ?, ?, ?)
ON CONFLICT(singleton) DO UPDATE SET head = excluded.head, tree = excluded.tree, manifest = excluded.manifest`, string(state.Head), state.Tree, manifest); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return writeWorkspace(ctx, s.dir, workspaceData)
}

func (s *Store) SavePublication(ctx context.Context, publication *Publication) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := LoadWorkspace(ctx, s.dir)
	if err != nil {
		return err
	}
	state.Publication = publication
	return saveWorkspace(ctx, s.dir, state)
}
