package apikey

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestKeysHave256BitSecretsAndVerify(t *testing.T) {
	seen := make(map[string]bool)
	for _, role := range []string{Admin, Machine} {
		for range 100 {
			key, err := Generate(role)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := Parse(key.Secret)
			if err != nil || parsed != key || seen[key.Secret] || !ValidID(key.ID) {
				t.Fatal("generated credential is invalid or duplicated")
			}
			seen[key.Secret] = true
			_, encoded, _ := strings.Cut(key.Secret, ".")
			secret, err := base64.RawURLEncoding.DecodeString(encoded)
			if err != nil || len(secret) != 32 {
				t.Fatal("secret does not contain 256 random bits")
			}
			digest := Digest(key.Secret)
			if len(digest) != 32 || !Verify(key.Secret, digest) || Verify(key.Secret+"wrong", digest) ||
				Verify(key.Secret, digest[:31]) {
				t.Fatal("digest verification is incorrect")
			}
		}
	}
}

func TestRejectMalformedKeys(t *testing.T) {
	key, err := Generate(Admin)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		"", "dsca_", key.Secret + " ", strings.TrimSuffix(key.Secret, key.Secret[len(key.Secret)-1:]),
		"dscx_" + key.Secret[5:], key.Secret[:5] + "!" + key.Secret[6:], key.Secret[:30] + "!" + key.Secret[31:],
	} {
		if _, err := Parse(value); err == nil {
			t.Fatal("malformed credential accepted")
		}
	}
	if _, err := Generate("other"); err == nil {
		t.Fatal("invalid role accepted")
	}
}
