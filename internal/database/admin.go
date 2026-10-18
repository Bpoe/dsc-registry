package database

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Bpoe/dsc-registry/internal/apikey"
	"github.com/google/uuid"
)

var (
	ErrNotFound            = errors.New("resource not found")
	ErrAlreadyBootstrapped = errors.New("a usable administrator key already exists")
)

type Principal struct {
	Role    string
	OwnerID string
	KeyID   string
}

type Machine struct {
	ID                    string `json:"id"`
	Name                  string `json:"name"`
	ConfigurationRevision int64  `json:"configurationRevision"`
}

type Key struct {
	KeyID       string  `json:"keyId"`
	OwnerType   string  `json:"ownerType"`
	OwnerID     string  `json:"ownerId"`
	CreatedAt   string  `json:"createdAt"`
	RevokedAt   *string `json:"revokedAt"`
	Description string  `json:"description"`
}

type IssuedKey struct {
	KeyID     string `json:"keyId"`
	Key       string `json:"key"`
	CreatedAt string `json:"createdAt"`
}

type BootstrapResult struct {
	AdminID string `json:"adminId"`
	IssuedKey
}

func (s *Store) Bootstrap(ctx context.Context, name string) (BootstrapResult, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return BootstrapResult{}, err
	}
	defer tx.Rollback()
	if s.Engine == "postgres" {
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(7286519402)"); err != nil {
			return BootstrapResult{}, err
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys k
		JOIN admins a ON a.id = k.owner_id
		WHERE k.owner_type = 'admin' AND k.revoked_at IS NULL`).Scan(&count); err != nil {
		return BootstrapResult{}, err
	}
	if count != 0 {
		return BootstrapResult{}, ErrAlreadyBootstrapped
	}
	var id string
	err = tx.QueryRowContext(ctx, "SELECT id FROM admins ORDER BY created_at, id LIMIT 1").Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		id = uuid.NewString()
		_, err = tx.ExecContext(ctx, "INSERT INTO admins(id, name, created_at) VALUES ($1, $2, $3)",
			id, name, timestamp())
	}
	if err != nil {
		return BootstrapResult{}, err
	}
	key, err := insertKey(ctx, tx, apikey.Admin, id, "local bootstrap")
	if err != nil {
		return BootstrapResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return BootstrapResult{}, err
	}
	return BootstrapResult{AdminID: id, IssuedKey: key}, nil
}

func (s *Store) Authenticate(ctx context.Context, value string) (Principal, error) {
	credential, err := apikey.Parse(value)
	if err != nil {
		return Principal{}, apikey.ErrInvalid
	}
	var role, owner string
	var digest []byte
	err = s.DB.QueryRowContext(ctx, `SELECT owner_type, owner_id, secret_hash FROM api_keys
		WHERE key_id = $1 AND revoked_at IS NULL`, credential.ID).Scan(&role, &owner, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, apikey.ErrInvalid
	}
	if err != nil {
		return Principal{}, err
	}
	if !apikey.Verify(value, digest) || credential.Role != role {
		return Principal{}, apikey.ErrInvalid
	}
	return Principal{Role: role, OwnerID: owner, KeyID: credential.ID}, nil
}

func (s *Store) CreateMachine(ctx context.Context, name string) (Machine, error) {
	machine := Machine{ID: uuid.NewString(), Name: name}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Machine{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO machines(id, name, configuration_revision, created_at)
		VALUES ($1, $2, 0, $3)`, machine.ID, name, timestamp()); err != nil {
		return Machine{}, err
	}
	if err := tx.Commit(); err != nil {
		return Machine{}, err
	}
	return machine, nil
}

func (s *Store) GetMachine(ctx context.Context, id string) (Machine, error) {
	var machine Machine
	err := s.DB.QueryRowContext(ctx, `SELECT id, name, configuration_revision FROM machines WHERE id = $1`, id).
		Scan(&machine.ID, &machine.Name, &machine.ConfigurationRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return Machine{}, ErrNotFound
	}
	return machine, err
}

func (s *Store) ListMachines(ctx context.Context, after string, limit int) ([]Machine, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, name, configuration_revision FROM machines
		WHERE id > $1 ORDER BY id LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	machines := make([]Machine, 0)
	for rows.Next() {
		var machine Machine
		if err := rows.Scan(&machine.ID, &machine.Name, &machine.ConfigurationRevision); err != nil {
			return nil, err
		}
		machines = append(machines, machine)
	}
	return machines, rows.Err()
}

func (s *Store) DeleteMachine(ctx context.Context, id string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "DELETE FROM machines WHERE id = $1", id)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil {
		return err
	} else if count == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) lockOwner(ctx context.Context, tx *sql.Tx, role, id string) error {
	table := "machines"
	if role == apikey.Admin {
		table = "admins"
	} else if role != apikey.Machine {
		return ErrNotFound
	}
	query := "SELECT id FROM " + table + " WHERE id = $1"
	if s.Engine == "postgres" {
		query += " FOR UPDATE"
	}
	var found string
	err := tx.QueryRowContext(ctx, query, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *Store) IssueKey(ctx context.Context, role, id, description string) (IssuedKey, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return IssuedKey{}, err
	}
	defer tx.Rollback()
	if err := s.lockOwner(ctx, tx, role, id); err != nil {
		return IssuedKey{}, err
	}
	key, err := insertKey(ctx, tx, role, id, description)
	if err != nil {
		return IssuedKey{}, err
	}
	if err := tx.Commit(); err != nil {
		return IssuedKey{}, err
	}
	return key, nil
}

func insertKey(ctx context.Context, tx *sql.Tx, role, id, description string) (IssuedKey, error) {
	credential, err := apikey.Generate(role)
	if err != nil {
		return IssuedKey{}, err
	}
	key := IssuedKey{KeyID: credential.ID, Key: credential.Secret, CreatedAt: timestamp()}
	_, err = tx.ExecContext(ctx, `INSERT INTO api_keys
		(key_id, owner_type, owner_id, secret_hash, created_at, description)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		key.KeyID, role, id, apikey.Digest(key.Key), key.CreatedAt, description)
	return key, err
}

func (s *Store) ListKeys(ctx context.Context, role, id, after string, limit int) ([]Key, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := s.lockOwner(ctx, tx, role, id); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT key_id, owner_type, owner_id, created_at, revoked_at, COALESCE(description, '')
		FROM api_keys WHERE owner_type = $1 AND owner_id = $2 AND key_id > $3 ORDER BY key_id LIMIT $4`,
		role, id, after, limit)
	if err != nil {
		return nil, err
	}
	keys := make([]Key, 0)
	for rows.Next() {
		var key Key
		if err := rows.Scan(&key.KeyID, &key.OwnerType, &key.OwnerID, &key.CreatedAt, &key.RevokedAt, &key.Description); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, key)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return keys, nil
}

func (s *Store) RevokeKey(ctx context.Context, role, id, keyID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.lockOwner(ctx, tx, role, id); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE api_keys SET revoked_at = COALESCE(revoked_at, $1)
		WHERE key_id = $2 AND owner_type = $3 AND owner_id = $4`, timestamp(), keyID, role, id)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil {
		return err
	} else if count == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func timestamp() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
