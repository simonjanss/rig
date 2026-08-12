// Package password hashes and verifies passwords with argon2id.
//
// Three decisions are worth knowing about before reading further.
//
// The stored value is a PHC string — the same format argon2's reference tools
// print — so a hash carries its own salt and cost and can be verified by
// anything that speaks the format, including a future version of this package
// that has raised the cost.
//
// The cost is stored alongside it in its own columns, which looks redundant and
// is not: "which accounts still have a hash below the current cost" has to be a
// query rather than a scan through a parser, or nobody ever runs it.
//
// And verification reports whether the stored hash is now too cheap, so login
// can quietly upgrade it. A cost you can raise but never apply to existing
// accounts is a cost you have not raised.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Algorithm is the only one this package writes. It is stored explicitly so
// that adding a second one later is a migration rather than an archaeology
// project.
const Algorithm = "argon2id"

// Params are argon2id's cost parameters.
type Params struct {
	// Memory is the working set in KiB. It is the parameter that actually
	// costs an attacker something: iterations can be parallelized across
	// cheap cores, memory cannot.
	Memory uint32 `json:"memory"`
	// Iterations is how many passes over that memory.
	Iterations uint32 `json:"iterations"`
	// Parallelism is how many lanes. It should not exceed the cores a request
	// may reasonably occupy.
	Parallelism uint8 `json:"parallelism"`
	// SaltLength and KeyLength are in bytes.
	SaltLength uint32 `json:"salt_length"`
	KeyLength  uint32 `json:"key_length"`
}

// DefaultParams follow the OWASP recommendation for argon2id: 64 MiB, three
// passes, two lanes.
//
// They cost roughly 50ms on a current server, which is the point — it is slow
// enough to make an offline guess expensive and fast enough that a login does
// not feel like one. Raise Memory first if you raise anything.
func DefaultParams() Params {
	return Params{
		Memory:      64 * 1024,
		Iterations:  3,
		Parallelism: 2,
		SaltLength:  16,
		KeyLength:   32,
	}
}

// AtLeast reports whether p is at least as expensive as want.
//
// It is what decides a rehash, so it is deliberately conservative: a hash is
// only out of date when it is cheaper on a dimension that matters, not when it
// merely differs.
func (p Params) AtLeast(want Params) bool {
	return p.Memory >= want.Memory &&
		p.Iterations >= want.Iterations &&
		p.KeyLength >= want.KeyLength &&
		p.SaltLength >= want.SaltLength
}

// Credential is what a row stores.
type Credential struct {
	// Encoded is the PHC string. It is the whole truth: salt, cost, and hash.
	Encoded string
	// Algorithm and Params duplicate what Encoded already says, so that
	// finding every credential due for a rehash is an indexed query rather
	// than a scan.
	Algorithm string
	Params    Params
}

// Hasher hashes at a fixed cost.
type Hasher struct {
	params Params
	// random is swappable so a test can pin a salt. Nothing else touches it.
	random func([]byte) (int, error)

	dummyOnce sync.Once
	dummy     string
}

// New builds a hasher. Zero-valued parameters fall back to the defaults, so a
// caller who only wants to raise the memory can say just that.
func New(p Params) *Hasher {
	d := DefaultParams()
	if p.Memory == 0 {
		p.Memory = d.Memory
	}
	if p.Iterations == 0 {
		p.Iterations = d.Iterations
	}
	if p.Parallelism == 0 {
		p.Parallelism = d.Parallelism
	}
	if p.SaltLength == 0 {
		p.SaltLength = d.SaltLength
	}
	if p.KeyLength == 0 {
		p.KeyLength = d.KeyLength
	}
	return &Hasher{params: p, random: rand.Read}
}

// Params returns the cost this hasher writes.
func (h *Hasher) Params() Params { return h.params }

// Hash derives a credential from a plaintext password.
func (h *Hasher) Hash(plain string) (Credential, error) {
	salt := make([]byte, h.params.SaltLength)
	if _, err := h.random(salt); err != nil {
		return Credential{}, fmt.Errorf("password: read salt: %w", err)
	}

	key := argon2.IDKey([]byte(plain), salt,
		h.params.Iterations, h.params.Memory, h.params.Parallelism, h.params.KeyLength)

	return Credential{
		Encoded:   encode(h.params, salt, key),
		Algorithm: Algorithm,
		Params:    h.params,
	}, nil
}

// ErrMalformed means a stored hash could not be read.
//
// It is distinct from a wrong password on purpose: one is somebody typing, the
// other is a corrupted row, and treating the second as the first would leave an
// account permanently unable to log in with no sign of why.
var ErrMalformed = errors.New("password: the stored hash is not a valid argon2id string")

// Verify checks a password against a stored hash.
//
// The second return says the stored hash is cheaper than this hasher's cost, so
// a caller that has just been handed the plaintext should rehash and store it.
// That is the only moment it can: the plaintext is never available again.
func (h *Hasher) Verify(encoded, plain string) (ok bool, needsRehash bool, err error) {
	params, salt, want, err := decode(encoded)
	if err != nil {
		return false, false, err
	}

	got := argon2.IDKey([]byte(plain), salt,
		params.Iterations, params.Memory, params.Parallelism, uint32(len(want)))

	// Constant time, because the comparison is over a value an attacker
	// partially controls and a byte-at-a-time answer is a byte-at-a-time leak.
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, false, nil
	}
	return true, !params.AtLeast(h.params), nil
}

// Dummy returns a hash to verify against when there is no such account.
//
// Verifying it is how a login for an unknown address spends the same time as
// one for a known address. Skipping the work turns response time into a
// membership oracle, and an attacker who can enumerate your users has done most
// of the work already.
//
// It is derived once, at this hasher's cost, so it stays a fair stand-in when
// the cost is raised.
func (h *Hasher) Dummy() string {
	h.dummyOnce.Do(func() {
		if c, err := h.Hash("this password belongs to no account"); err == nil {
			h.dummy = c.Encoded
		}
	})
	return h.dummy
}

// encode renders a credential in PHC string format.
func encode(p Params, salt, key []byte) string {
	return fmt.Sprintf("$%s$v=%d$%s$%s$%s",
		Algorithm, argon2.Version, costSegment(p),
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

func costSegment(p Params) string {
	return fmt.Sprintf("m=%d,t=%d,p=%d", p.Memory, p.Iterations, p.Parallelism)
}

// decode reads a PHC string.
func decode(encoded string) (Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// A leading $ makes the first part empty, so a well-formed string has six.
	if len(parts) != 6 || parts[0] != "" {
		return Params{}, nil, nil, ErrMalformed
	}
	if parts[1] != Algorithm {
		return Params{}, nil, nil, fmt.Errorf("%w: algorithm is %q", ErrMalformed, parts[1])
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return Params{}, nil, nil, ErrMalformed
	}
	if version != argon2.Version {
		return Params{}, nil, nil, fmt.Errorf("%w: version %d is not supported", ErrMalformed, version)
	}

	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Iterations, &p.Parallelism); err != nil {
		return Params{}, nil, nil, ErrMalformed
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, ErrMalformed
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return Params{}, nil, nil, ErrMalformed
	}

	p.SaltLength = uint32(len(salt))
	p.KeyLength = uint32(len(key))
	return p, salt, key, nil
}
