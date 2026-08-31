package electric

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
)

// The verifier, pinned against a published vector rather than against itself.
//
// The salt and password are RFC 7677 §3's, and the expected string was derived
// independently — the same four steps in Python — from that vector. What makes the
// derivation trustworthy rather than merely reproducible is that the same ServerKey it
// produces regenerates the ServerSignature the RFC publishes for its example exchange.
//
// If this fails, the sync service cannot authenticate: Postgres stores whatever this
// produces verbatim, so a wrong verifier is a role whose password is a string nobody knows.
func TestScramVerifierMatchesAPublishedVector(t *testing.T) {
	salt, err := base64.StdEncoding.DecodeString("W22ZaJ0SNY7soEsUEjb6gQ==")
	if err != nil {
		t.Fatal(err)
	}

	const want = "SCRAM-SHA-256$4096:W22ZaJ0SNY7soEsUEjb6gQ==$" +
		"WG5d8oPm3OtcPnkdi4Uo7BkeZkBFzpcXkuLmtbsT4qY=:wfPLwcE6nTWhTAmQ7tl2KeoiWGPlZqQxSrmfPwDl2dU="

	got, err := scramVerifierWithSalt("pencil", salt)
	if err != nil {
		t.Fatalf("scramVerifierWithSalt: %v", err)
	}
	if got != want {
		t.Errorf("scramVerifierWithSalt() =\n  %s\nwant\n  %s", got, want)
	}
}

// A fresh salt every time, or two environments built from the same password would carry
// the same verifier — which is the property salting exists for.
func TestScramVerifierSaltsEachCall(t *testing.T) {
	first, err := scramVerifier("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	second, err := scramVerifier("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("two calls produced the same verifier, so the salt is not random")
	}
	if !strings.HasPrefix(first, "SCRAM-SHA-256$4096:") {
		t.Errorf("verifier does not carry the prefix Postgres parses: %s", first)
	}
}

// The whole point of the empty case: this runs on every boot, and most boots are on a
// laptop or in an environment with no sync service. A nil database is the assertion —
// nothing may be touched before the variable has been looked at.
func TestSetRolePasswordDoesNothingWithoutADSN(t *testing.T) {
	set, err := SetRolePassword(context.Background(), nil, "", Role)
	if err != nil {
		t.Errorf("an unset connection string should be silent, got %v", err)
	}
	if set {
		t.Error("it reported setting a password it was never given")
	}
}

// Every one of these has to be caught before the database is reached, which is why a nil
// one is safe to pass: touching it would panic rather than fail quietly.
func TestSetRolePasswordRejectsUnusableConnectionStrings(t *testing.T) {
	const secret = "s3cretpassword"

	for _, tc := range []struct {
		name string
		dsn  string
	}{
		// net/url's own parse error quotes the whole URL back, password and all,
		// which is why SetRolePassword does not wrap it. This case is as much a test
		// of that as of the rejection.
		{"not a URL", "postgresql://electric:" + secret + "%zz@host:5432/app"},
		{"no password", "postgresql://electric@host:5432/app"},
		// The case that matters most: a connection string naming the master user
		// would otherwise reset the master password.
		{"another role", "postgresql://app_admin:" + secret + "@host:5432/app"},
		// SCRAM's normalisation is the identity map only on the alphanumeric
		// alphabet, so whatever generates the password has to stay inside it.
		{"password outside the alphabet SCRAM normalisation assumes",
			"postgresql://electric:with%20a%20space@host:5432/app"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set, err := SetRolePassword(context.Background(), nil, tc.dsn, Role)
			if err == nil {
				t.Fatal("accepted a connection string it cannot set a password from")
			}
			if set {
				t.Error("it reported setting a password and also failed")
			}
			// The rule the whole file is written to: an error here is logged, and a
			// password in a log is the thing this is all avoiding.
			if strings.Contains(err.Error(), secret) {
				t.Errorf("the error carries the password: %v", err)
			}
		})
	}
}

// A caller that passes no role at all is a caller that has not decided which one, and
// guessing [Role] for it would be the one place this file invents a target.
func TestSetRolePasswordRefusesAnEmptyRole(t *testing.T) {
	if _, err := SetRolePassword(context.Background(), nil, "postgresql://electric:pw@host:5432/app", ""); err == nil {
		t.Fatal("accepted an empty role")
	}
}
