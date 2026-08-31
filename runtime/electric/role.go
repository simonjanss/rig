// The credential the sync service authenticates to Postgres with.
//
// rig's electric/foundation set creates [Role] with LOGIN and no password at all, which is
// the whole reason that SQL can be a file in git: nothing in it is a credential, and the
// role cannot connect until something gives it one. This is that something. It runs once at
// boot, straight after the migrations, and takes the password out of the same secret the
// sync service itself reads as DATABASE_URL — one secret with two readers rather than a
// second one holding a duplicate that can drift out of step with it.
//
// It costs nobody a dependency. The secret arrives as an ordinary environment variable,
// resolved by whatever runs the process, so a project doing this carries no cloud SDK for
// the sake of it.

package electric

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"

	"github.com/jackc/pgx/v5/pgconn"
)

// Role is the Postgres role rig's electric/foundation set creates, and the one
// [SetRolePassword] will set a password on.
//
// Stated here because two things read it and they are in different modules: the SQL that
// creates the role, and the caller that hands its connection string to [SetRolePassword].
const Role = "electric"

// scramIterations is PostgreSQL's own default for scram_iterations. Matching it means a
// verifier built here is indistinguishable from one the server would have built itself.
const scramIterations = 4096

// SetRolePassword gives role the password carried in dsn, and reports whether it did.
//
// An empty dsn does nothing and returns false, which is every environment without a sync
// service — a laptop, and any deployment that has not grown one. Absent means absent
// rather than broken, the same discipline the proxy's own URL follows.
//
// role is a parameter rather than [Role] read directly, and the check that dsn's username
// equals it is not fussiness. A connection string naming the database's master user would
// otherwise reset the master password, which is a way to lock a deployment out of its own
// database from a line that looks like configuration.
//
// It is deliberately not an ALTER ROLE with the password in it. A managed Postgres commonly
// logs DDL and captures statement text for performance insight, and Postgres redacts a
// PASSWORD literal in neither — which is why psql has \password. So what goes over the wire
// and into those logs is a SCRAM-SHA-256 verifier, which the server stores verbatim when a
// PASSWORD literal already parses as one. Unlike an MD5 hash a SCRAM verifier is not
// password-equivalent: it holds StoredKey, which lets a server verify a client's proof but
// not produce one, so it cannot be replayed to log in. What is left is an offline attack on
// 4096 rounds of PBKDF2 against whatever generated the password.
//
// No error returned from here carries the DSN, the password or the verifier.
//
// There is no logger, because this package has none — see [Config.OnError]. The bool is
// what a caller logs off: true is worth one line at boot, false is worth silence.
func SetRolePassword(ctx context.Context, db DB, dsn, role string) (bool, error) {
	if dsn == "" {
		return false, nil
	}
	if role == "" {
		return false, errors.New("electric: no role to set a password on")
	}

	u, err := url.Parse(dsn)
	if err != nil {
		// Not %w, and not the string: a parse error on a URL carries the URL.
		return false, errors.New("electric: the sync service's connection string is not a URL")
	}
	if got := u.User.Username(); got != role {
		return false, fmt.Errorf("electric: the sync service's connection string connects as %q, but the role this sets a password on is %q", got, role)
	}
	password, ok := u.User.Password()
	if !ok || password == "" {
		return false, errors.New("electric: the sync service's connection string carries no password")
	}
	if !isASCIIAlphanumeric(password) {
		// SASLprep is the identity map on this alphabet and is not the identity map in
		// general, so a password outside it would need normalising before hashing or the
		// verifier would not match what the server derives from the same password.
		// Refusing is what stops the two drifting apart quietly: whatever generates the
		// password has to stay inside the alphabet, and this is where it finds out.
		return false, errors.New("electric: the sync service's password is not alphanumeric, which SCRAM normalisation here assumes")
	}

	verifier, err := scramVerifier(password)
	if err != nil {
		return false, err
	}

	// ALTER ROLE takes no parameters, so the verifier has to end up inside the statement
	// text. Letting the server quote it — with the value bound as a parameter to this
	// query, so it is not in *this* statement's text either — rather than concatenating it
	// here. The alphabet a verifier can contain makes that safe either way; the point is
	// not to have a place where it depends on that being true.
	var stmt string
	if err := db.QueryRow(ctx,
		`SELECT format('ALTER ROLE %I PASSWORD %L', $1::text, $2::text)`,
		role, verifier,
	).Scan(&stmt); err != nil {
		return false, fmt.Errorf("electric: quoting the role's password: %w", err)
	}

	if _, err := db.Exec(ctx, stmt); err != nil {
		// Reported field by field rather than wrapped. stmt holds the verifier, and an
		// error from a statement is a plausible place for that statement to appear — a
		// rule about what may be wrapped is only worth having if it is not conditional on
		// what some other library happens to put in a string today.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return false, fmt.Errorf("electric: setting the role's password: %s: %s", pgErr.Code, pgErr.Message)
		}
		return false, errors.New("electric: setting the role's password failed before Postgres answered")
	}

	return true, nil
}

// scramVerifier renders password as a SCRAM-SHA-256 verifier with a fresh salt, in the
// format PostgreSQL stores in pg_authid.rolpassword and accepts in ALTER ROLE ... PASSWORD:
//
//	SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>
//
// with the three binary fields base64-encoded. RFC 5802 §3 is where the derivation comes
// from; PostgreSQL's is that with SHA-256 and no channel binding.
func scramVerifier(password string) (string, error) {
	// 16 bytes is what PostgreSQL itself uses (SCRAM_DEFAULT_SALT_LEN).
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("electric: generating a salt: %w", err)
	}
	return scramVerifierWithSalt(password, salt)
}

// scramVerifierWithSalt is scramVerifier with the one non-deterministic input lifted out,
// which is what makes it testable against a published vector.
func scramVerifierWithSalt(password string, salt []byte) (string, error) {
	saltedPassword, err := pbkdf2.Key(sha256.New, password, salt, scramIterations, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("electric: deriving the salted password: %w", err)
	}

	clientKey := hmacSHA256(saltedPassword, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := hmacSHA256(saltedPassword, "Server Key")

	// StoredKey rather than ClientKey, and that is the property this whole file rests on:
	// SHA-256 is not invertible, so a verifier does not yield the ClientKey a login needs.
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s",
		scramIterations, b64(salt), b64(storedKey[:]), b64(serverKey)), nil
}

func hmacSHA256(key []byte, message string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(message))
	return mac.Sum(nil)
}

func isASCIIAlphanumeric(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return s != ""
}
