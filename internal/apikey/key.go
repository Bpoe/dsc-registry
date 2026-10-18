package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
)

const (
	Admin   = "admin"
	Machine = "machine"
)

var ErrInvalid = errors.New("invalid API key")

type Credential struct {
	ID     string
	Role   string
	Secret string
}

func Generate(role string) (Credential, error) {
	prefix := "dscm_"
	if role == Admin {
		prefix = "dsca_"
	} else if role != Machine {
		return Credential{}, ErrInvalid
	}
	id := make([]byte, 16)
	secret := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		return Credential{}, err
	}
	if _, err := rand.Read(secret); err != nil {
		return Credential{}, err
	}
	keyID := "k_" + base64.RawURLEncoding.EncodeToString(id)
	return Credential{ID: keyID, Role: role,
		Secret: prefix + keyID + "." + base64.RawURLEncoding.EncodeToString(secret)}, nil
}

func Parse(value string) (Credential, error) {
	if len(value) != 73 || (value[:5] != "dsca_" && value[:5] != "dscm_") {
		return Credential{}, ErrInvalid
	}
	id, secret, ok := strings.Cut(value[5:], ".")
	if !ok || !ValidID(id) || !validEncoding(secret, 32) {
		return Credential{}, ErrInvalid
	}
	role := Machine
	if value[:5] == "dsca_" {
		role = Admin
	}
	return Credential{ID: id, Role: role, Secret: value}, nil
}

func ValidID(id string) bool {
	return strings.HasPrefix(id, "k_") && validEncoding(strings.TrimPrefix(id, "k_"), 16)
}

func validEncoding(value string, size int) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == size && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func Digest(secret string) []byte {
	digest := sha256.Sum256([]byte(secret))
	return digest[:]
}

func Verify(secret string, digest []byte) bool {
	return subtle.ConstantTimeCompare(Digest(secret), digest) == 1
}
