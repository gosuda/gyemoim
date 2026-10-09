// Password hashing for management logins. The store stays hash-agnostic: it
// persists whatever encoded hash string callers hand it, and this file is the
// only place that understands the hash format.
package config

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters for a low-traffic personal tool: 64 MiB of memory,
// three passes, one lane. Stored in the encoded hash so future updates can
// raise them while verification keeps accepting older hashes.
const (
	passwordHashMemoryKiB = 64 * 1024
	passwordHashTime      = 3
	passwordHashThreads   = 1
	passwordSaltLength    = 16
	passwordKeyLength     = 32

	// Verification refuses parameter combinations outside these bounds instead
	// of passing them to argon2.IDKey, which panics below its own minimums and
	// would truncate threads to uint8; the memory cap also keeps a corrupted or
	// hand-crafted stored hash from requesting an absurd allocation.
	passwordHashMaxThreads   = 255
	passwordHashMaxMemoryKiB = 1 << 20 // 1 GiB
)

// HashPassword derives an argon2id PHC-encoded hash string with a fresh random
// salt, e.g. $argon2id$v=19$m=65536,t=3,p=1$<b64salt>$<b64hash>.
func HashPassword(password string) (string, error) {
	salt := make([]byte, passwordSaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, passwordHashTime, passwordHashMemoryKiB, passwordHashThreads, passwordKeyLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, passwordHashMemoryKiB, passwordHashTime, passwordHashThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches a stored argon2id hash.
// Malformed stored hashes — including parameter values the library would panic
// on — are a verification failure, never a panic.
func VerifyPassword(password, encoded string) bool {
	fields := strings.Split(encoded, "$")
	if len(fields) != 6 || fields[0] != "" || fields[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(fields[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var memory, time, threads uint32
	if _, err := fmt.Sscanf(fields[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false
	}
	if time < 1 || threads < 1 || threads > passwordHashMaxThreads || memory < 8*threads || memory > passwordHashMaxMemoryKiB {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(fields[4])
	if err != nil || len(salt) == 0 {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(fields[5])
	if err != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, time, memory, uint8(threads), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}
