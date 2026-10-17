package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// ErrConflict is a write whose base hash no longer matches the file.
var ErrConflict = errors.New("the file changed since it was loaded")

// ErrInvalid is agent configuration that does not validate.
var ErrInvalid = errors.New("invalid agent configuration")

// Hash returns the hex sha256 of body.
func Hash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}
