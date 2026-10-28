package auth

import (
	"encoding/base64"
	"fmt"
	"math/rand/v2"
	"testing"

	"golang.org/x/crypto/argon2"
)

func TestPasswordVerificationReportsCurrentParameters(t *testing.T) {
	const password = "parameter agility regression"
	current, err := HashPassword(password, rand.NewChaCha8([32]byte{42}))
	if err != nil {
		t.Fatal(err)
	}
	salt := []byte("0123456789abcdef")
	digest := argon2.IDKey([]byte(password), salt, 1, 19456, 2, 32)
	outdated := fmt.Sprintf("$argon2id$v=19$m=19456,t=1,p=2$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(digest))
	for _, test := range []struct {
		name, hash string
		current    bool
	}{{"current", current, true}, {"outdated", outdated, false}} {
		t.Run(test.name, func(t *testing.T) {
			matches, current, err := verifyPassword(password, test.hash)
			if err != nil || !matches || current != test.current {
				t.Fatalf("verification = %v, current=%v, %v; want match and current=%v", matches, current, err, test.current)
			}
		})
	}
}
