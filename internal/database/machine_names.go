package database

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var ErrMachineNameConflict = errors.New("machine name already exists")

func foldMachineName(name string) string {
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(name)))
}

func machineNameConflict(err error) bool {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		return postgresError.Code == "23505" && postgresError.ConstraintName == "machines_name_folded_unique"
	}
	var sqliteError *sqlite.Error
	// CreateMachine's only secondary unique index is the folded name. Primary
	// key collisions use SQLITE_CONSTRAINT_PRIMARYKEY, not this extended code.
	return errors.As(err, &sqliteError) && sqliteError.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}

func (s *Store) GetMachineByName(ctx context.Context, name string) (Machine, error) {
	var machine Machine
	err := s.DB.QueryRowContext(ctx, `SELECT id, name, configuration_revision FROM machines WHERE name_folded = $1`,
		foldMachineName(name)).Scan(&machine.ID, &machine.Name, &machine.ConfigurationRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return Machine{}, ErrNotFound
	}
	return machine, err
}

func backfillMachineNames(ctx context.Context, tx *sql.Tx) error {
	const batchSize = 100
	for {
		rows, err := tx.QueryContext(ctx, `SELECT id, name FROM machines
			WHERE name_folded IS NULL ORDER BY id LIMIT $1`, batchSize)
		if err != nil {
			return err
		}
		batch := make([]Machine, 0, batchSize)
		for rows.Next() {
			var machine Machine
			if err := rows.Scan(&machine.ID, &machine.Name); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, machine)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, machine := range batch {
			if _, err := tx.ExecContext(ctx, "UPDATE machines SET name_folded = $1 WHERE id = $2",
				foldMachineName(machine.Name), machine.ID); err != nil {
				return err
			}
		}
		if len(batch) < batchSize {
			return nil
		}
	}
}
