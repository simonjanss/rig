package session

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Tokens are opaque, not JWTs.
//
// A JWT saves one indexed lookup per request and costs a signing key to rotate,
// a JWKS endpoint to serve, a clock-skew policy to argue about, and — the part
// that matters — a window during which a revoked session keeps working because
// nothing checks. Here, verification is a row read, so revocation takes effect
// on the next request and "log out everywhere" means what it says.
//
// The presented value is <prefix><base32(id)>.<base32(secret)>. The identifier
// half is the lookup key and the secret half is compared against a stored
// sha256. Splitting them is what keeps the lookup exact: a bare secret would
// have to be found by scanning, and a scan over hashes is not an index.
const (
	// PrefixRefresh and PrefixAccess make a leaked token identifiable on
	// sight, in a log or a paste, by whoever finds it.
	PrefixRefresh = "rig_rt_"
	PrefixAccess  = "rig_at_"

	secretBytes = 32
)

// encoding is base32 without padding: case-insensitive on the wire, no
// characters that need escaping in a header, a URL, or a shell.
var encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// secret is the half of a token that is never stored.
type secret []byte

// mint generates a token identifier and its secret.
func mint() (uuid.UUID, secret, error) {
	// A v7 identifier sorts by creation time, which keeps the index that every
	// verification hits from fragmenting the way random keys do.
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("session: generate token id: %w", err)
	}

	s := make([]byte, secretBytes)
	if _, err := rand.Read(s); err != nil {
		return uuid.Nil, nil, fmt.Errorf("session: generate token secret: %w", err)
	}
	return id, s, nil
}

// format renders the value handed to the client. It is the only moment the
// secret exists outside the caller's memory.
func format(kind Kind, id uuid.UUID, s secret) string {
	prefix := PrefixRefresh
	if kind == KindAccess {
		prefix = PrefixAccess
	}
	return prefix + encoding.EncodeToString(id[:]) + "." + encoding.EncodeToString(s)
}

// hash is what the row stores.
//
// sha256 rather than argon2id, deliberately. A password is short, human-chosen,
// and worth grinding for; a token secret is 256 bits of entropy, and no amount
// of guessing will find it. Running a memory-hard function on every request
// would be a denial of service aimed at ourselves.
func hashSecret(s secret) []byte {
	sum := sha256.Sum256(s)
	return sum[:]
}

// matches compares a presented secret against a stored hash in constant time.
func matches(stored []byte, s secret) bool {
	got := hashSecret(s)
	return subtle.ConstantTimeCompare(stored, got) == 1
}

// parse splits a presented token.
//
// Every failure returns the same error. Distinguishing "wrong prefix" from
// "bad base32" from "no such token" tells a caller which half of their guess
// was right, and nobody legitimate ever needs to know.
func parse(presented string) (Kind, uuid.UUID, secret, error) {
	var kind Kind
	body := ""

	switch {
	case strings.HasPrefix(presented, PrefixRefresh):
		kind, body = KindRefresh, strings.TrimPrefix(presented, PrefixRefresh)
	case strings.HasPrefix(presented, PrefixAccess):
		kind, body = KindAccess, strings.TrimPrefix(presented, PrefixAccess)
	default:
		return "", uuid.Nil, nil, ErrInvalidToken
	}

	rawID, rawSecret, found := strings.Cut(body, ".")
	if !found {
		return "", uuid.Nil, nil, ErrInvalidToken
	}

	idBytes, err := encoding.DecodeString(strings.ToUpper(rawID))
	if err != nil || len(idBytes) != 16 {
		return "", uuid.Nil, nil, ErrInvalidToken
	}
	s, err := encoding.DecodeString(strings.ToUpper(rawSecret))
	if err != nil || len(s) != secretBytes {
		return "", uuid.Nil, nil, ErrInvalidToken
	}

	return kind, uuid.UUID(idBytes), s, nil
}
