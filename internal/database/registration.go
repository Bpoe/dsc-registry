package database

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"

	"github.com/Bpoe/dsc-registry/internal/apikey"
)

const (
	DefaultRegistrationTTLSeconds = 3600
	MaxRegistrationTTLSeconds     = 86400
)

var (
	ErrInvalidRegistrationTTL   = errors.New("invalid registration token TTL")
	ErrInvalidRegistrationToken = errors.New("invalid registration token")
)

type RegistrationToken struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expiresAt"`
}

type RegisteredMachine struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	APIKey string `json:"apiKey"`
}

func (s *Store) CreateRegistrationToken(ctx context.Context, ttlSeconds int64) (RegistrationToken, error) {
	if ttlSeconds < 1 || ttlSeconds > MaxRegistrationTTLSeconds {
		return RegistrationToken{}, ErrInvalidRegistrationTTL
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return RegistrationToken{}, err
	}
	result := RegistrationToken{
		Token:     "dscr_" + base64.RawURLEncoding.EncodeToString(secret),
		ExpiresAt: time.Now().UTC().Add(time.Duration(ttlSeconds) * time.Second).Format(time.RFC3339Nano),
	}
	digest := sha256.Sum256([]byte(result.Token))
	tx, err := s.begin(ctx, "create_registration_token")
	if err != nil {
		return RegistrationToken{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO registration_tokens(token_hash, expires_at) VALUES ($1, $2)",
		digest[:], result.ExpiresAt); err != nil {
		return RegistrationToken{}, err
	}
	if err := tx.Commit(); err != nil {
		return RegistrationToken{}, err
	}
	return result, nil
}

func (s *Store) AuthenticateRegistrationToken(ctx context.Context, value string) error {
	digest := sha256.Sum256([]byte(value))
	var expiration string
	err := s.DB.QueryRowContext(ctx, "SELECT expires_at FROM registration_tokens WHERE token_hash = $1", digest[:]).
		Scan(&expiration)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidRegistrationToken
	}
	if err != nil {
		return err
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, expiration)
	if err != nil {
		return err
	}
	if !time.Now().UTC().Before(expiresAt) {
		return ErrInvalidRegistrationToken
	}
	return nil
}

func (s *Store) CreateMachineWithKey(ctx context.Context, name string) (RegisteredMachine, error) {
	tx, err := s.begin(ctx, "register_machine")
	if err != nil {
		return RegisteredMachine{}, err
	}
	defer tx.Rollback()
	machine, err := insertMachine(ctx, tx.Tx, name)
	if err != nil {
		return RegisteredMachine{}, err
	}
	key, err := insertKey(ctx, tx.Tx, apikey.Machine, machine.ID, "")
	if err != nil {
		return RegisteredMachine{}, err
	}
	if err := tx.Commit(); err != nil {
		return RegisteredMachine{}, err
	}
	return RegisteredMachine{ID: machine.ID, Name: machine.Name, APIKey: key.Key}, nil
}
