package database

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"

	"github.com/Bpoe/dsc-registry/internal/apikey"
	"github.com/Bpoe/dsc-registry/internal/desired"
	"github.com/google/uuid"
)

var ErrConfigurationNotFound = errors.New("configuration not found")

type ConfigurationSummary struct {
	Name     string `json:"name"`
	Revision string `json:"revision"`
}

type Configuration struct {
	ConfigurationSummary
	desired.Content
}

type ConfigurationResult struct {
	Configuration
	MachineConfigurationRevision int64 `json:"machineConfigurationRevision"`
}

type ConfigurationSnapshot struct {
	ConfigurationRevision int64           `json:"configurationRevision"`
	Configurations        []Configuration `json:"configurations"`
}

func (s *Store) PutConfiguration(ctx context.Context, id, name string, content desired.Content) (ConfigurationResult, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return ConfigurationResult{}, false, err
	}
	defer tx.Rollback()
	if err := s.lockOwner(ctx, tx, apikey.Machine, id); err != nil {
		return ConfigurationResult{}, false, err
	}
	current, err := getConfiguration(ctx, tx, id, name)
	created := errors.Is(err, ErrConfigurationNotFound)
	if err != nil && !created {
		return ConfigurationResult{}, false, err
	}
	if created || !bytes.Equal(current.Template, content.Template) || !bytes.Equal(current.Parameters, content.Parameters) {
		current = Configuration{ConfigurationSummary: ConfigurationSummary{Name: name, Revision: uuid.NewString()}, Content: content}
		// A NUL separator cannot occur literally in valid JSON; it also marks
		// absent parameters distinctly from a present empty object.
		hash := sha256.New()
		hash.Write(content.Template)
		hash.Write([]byte{0})
		hash.Write(content.Parameters)
		var parameters any
		if content.Parameters != nil {
			parameters = string(content.Parameters)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO configurations
			(machine_id, name, revision, template_json, parameters_json, content_sha256, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (machine_id, name) DO UPDATE SET revision = excluded.revision,
			template_json = excluded.template_json, parameters_json = excluded.parameters_json,
			content_sha256 = excluded.content_sha256, updated_at = excluded.updated_at`,
			id, name, current.Revision, string(content.Template), parameters, hash.Sum(nil), timestamp())
		if err != nil {
			return ConfigurationResult{}, false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE machines SET configuration_revision = configuration_revision + 1 WHERE id = $1`, id); err != nil {
			return ConfigurationResult{}, false, err
		}
	}
	result := ConfigurationResult{Configuration: current}
	if err := tx.QueryRowContext(ctx, "SELECT configuration_revision FROM machines WHERE id = $1", id).
		Scan(&result.MachineConfigurationRevision); err != nil {
		return ConfigurationResult{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return ConfigurationResult{}, false, err
	}
	return result, created, nil
}

func getConfiguration(ctx context.Context, tx *sql.Tx, id, name string) (Configuration, error) {
	var item Configuration
	var template string
	var parameters sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT name, revision, template_json, parameters_json
		FROM configurations WHERE machine_id = $1 AND name = $2`, id, name).
		Scan(&item.Name, &item.Revision, &template, &parameters)
	if errors.Is(err, sql.ErrNoRows) {
		return Configuration{}, ErrConfigurationNotFound
	}
	item.Template = []byte(template)
	if parameters.Valid {
		item.Parameters = []byte(parameters.String)
	}
	return item, err
}

func (s *Store) GetConfiguration(ctx context.Context, id, name string) (ConfigurationResult, error) {
	// A single statement returns desired content and its collection revision
	// from the same MVCC snapshot on both engines.
	var result ConfigurationResult
	var foundName, revision, template, parameters sql.NullString
	err := s.DB.QueryRowContext(ctx, `SELECT m.configuration_revision, c.name, c.revision, c.template_json, c.parameters_json
		FROM machines m LEFT JOIN configurations c ON c.machine_id = m.id AND c.name = $2
		WHERE m.id = $1`, id, name).
		Scan(&result.MachineConfigurationRevision, &foundName, &revision, &template, &parameters)
	if errors.Is(err, sql.ErrNoRows) {
		return ConfigurationResult{}, ErrNotFound
	}
	if err != nil {
		return ConfigurationResult{}, err
	}
	if !foundName.Valid {
		return ConfigurationResult{}, ErrConfigurationNotFound
	}
	result.Configuration = Configuration{
		ConfigurationSummary: ConfigurationSummary{Name: foundName.String, Revision: revision.String},
		Content:              desired.Content{Template: []byte(template.String)},
	}
	if parameters.Valid {
		result.Parameters = []byte(parameters.String)
	}
	return result, nil
}

func (s *Store) ListConfigurations(ctx context.Context, id, after string, limit int) ([]ConfigurationSummary, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT c.name, c.revision FROM machines m
		LEFT JOIN (SELECT name, revision, machine_id FROM configurations
			WHERE machine_id = $1 AND name > $2 ORDER BY name LIMIT $3) c ON c.machine_id = m.id
		WHERE m.id = $1 ORDER BY c.name`, id, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ConfigurationSummary, 0)
	found := false
	for rows.Next() {
		found = true
		var name, revision sql.NullString
		if err := rows.Scan(&name, &revision); err != nil {
			return nil, err
		}
		if name.Valid {
			items = append(items, ConfigurationSummary{Name: name.String, Revision: revision.String})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrNotFound
	}
	return items, nil
}

func (s *Store) DeleteConfiguration(ctx context.Context, id, name string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.lockOwner(ctx, tx, apikey.Machine, id); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM configurations WHERE machine_id = $1 AND name = $2", id, name)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil {
		return err
	} else if count == 0 {
		return ErrConfigurationNotFound
	}
	if _, err := tx.ExecContext(ctx, "UPDATE machines SET configuration_revision = configuration_revision + 1 WHERE id = $1", id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ConfigurationSnapshot(ctx context.Context, id string) (ConfigurationSnapshot, error) {
	// One ordered SELECT prevents mixing a revision with another transaction's
	// content, including an empty collection during concurrent deletion.
	rows, err := s.DB.QueryContext(ctx, `SELECT m.configuration_revision, c.name, c.revision, c.template_json, c.parameters_json
		FROM machines m LEFT JOIN configurations c ON c.machine_id = m.id WHERE m.id = $1 ORDER BY c.name`, id)
	if err != nil {
		return ConfigurationSnapshot{}, err
	}
	defer rows.Close()
	snapshot := ConfigurationSnapshot{Configurations: make([]Configuration, 0)}
	found := false
	for rows.Next() {
		found = true
		var name, revision, template, parameters sql.NullString
		if err := rows.Scan(&snapshot.ConfigurationRevision, &name, &revision, &template, &parameters); err != nil {
			return ConfigurationSnapshot{}, err
		}
		if name.Valid {
			item := Configuration{
				ConfigurationSummary: ConfigurationSummary{Name: name.String, Revision: revision.String},
				Content:              desired.Content{Template: []byte(template.String)},
			}
			if parameters.Valid {
				item.Parameters = []byte(parameters.String)
			}
			snapshot.Configurations = append(snapshot.Configurations, item)
		}
	}
	if err := rows.Err(); err != nil {
		return ConfigurationSnapshot{}, err
	}
	if !found {
		return ConfigurationSnapshot{}, ErrNotFound
	}
	return snapshot, nil
}
