package config

import (
	"strings"
	"testing"
)

// This function had no test at all, which is the whole reason the bug it
// shipped with reached a boot. The regression below is not "the mapping is
// nice" — it is "the manager starts", and the only honest way to keep that
// true is to name the driver's accepted vocabulary in a test that fails when
// the two drift apart.

// TestTheComposedDSNIsOneTheDriverAccepts is the one that was missing. The
// default sslmode is "disable" and libpq's "disable" is not a value
// go-sql-driver knows, so the DSN the Helm path composes was rejected at
// open time with an error that named neither the field nor the fix.
func TestTheComposedDSNIsOneTheDriverAccepts(t *testing.T) {
	for _, mode := range []string{"", "disable", "allow", "prefer", "require", "verify-ca", "verify-full"} {
		dsn, err := buildMySQLDSN(DBConfig{
			Host: "db.internal", Port: 3306,
			User: "opskeeper", Password: "secret", DBName: "opskeeper",
			SSLMode: mode,
		})
		if err != nil {
			t.Fatalf("sslmode %q: %v", mode, err)
		}
		tls := dsnParam(t, dsn, "tls")
		// Exactly what go-sql-driver's parseDSNParams switches on. An
		// empty value means the parameter is absent, which is the only
		// way to say "plain" — "disable" is not one of the four.
		switch tls {
		case "", "true", "skip-verify", "preferred":
		default:
			t.Errorf("sslmode %q composed tls=%q, which go-sql-driver rejects with "+
				"\"invalid value / unknown config name\"", mode, tls)
		}
		if !strings.HasPrefix(dsn, "opskeeper:secret@tcp(db.internal:3306)/opskeeper?") {
			t.Errorf("dsn = %q; the discrete fields are not composed as documented", dsn)
		}
	}
}

// The two rows that carry a decision rather than a spelling.
func TestSSLModeIsTranslatedRatherThanPasted(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want string
		why  string
	}{
		{"disable", "", "plain: the parameter must be absent, not present-and-false"},
		{"require", "skip-verify", "libpq's require does not verify a certificate; " +
			"mapping it to true would be a silent policy change that fails closed on a self-signed cert"},
		{"verify-full", "true", "verify against the system roots; ServerName comes from the host"},
		{"prefer", "preferred", "try TLS, fall back to plaintext"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			dsn, err := buildMySQLDSN(DBConfig{Host: "h", User: "u", DBName: "d", SSLMode: tc.mode})
			if err != nil {
				t.Fatalf("buildMySQLDSN: %v", err)
			}
			if got := dsnParam(t, dsn, "tls"); got != tc.want {
				t.Errorf("sslmode %q -> tls=%q, want %q: %s", tc.mode, got, tc.want, tc.why)
			}
		})
	}
}

// An operator who already worked around the old bug by writing the driver's
// own spelling must not be broken by the fix.
func TestTheDriversOwnSpellingsAreStillAccepted(t *testing.T) {
	for _, mode := range []string{"false", "true", "skip-verify", "preferred", "SKIP-VERIFY", " require "} {
		if _, err := buildMySQLDSN(DBConfig{Host: "h", User: "u", DBName: "d", SSLMode: mode}); err != nil {
			t.Errorf("sslmode %q was rejected: %v", mode, err)
		}
	}
}

// An sslmode that does not exist is a question this code cannot answer. The
// answer must not be "connect in plaintext", because that is the expensive
// direction and it is silent.
func TestAnUnknownSSLModeIsRefusedByName(t *testing.T) {
	_, err := buildMySQLDSN(DBConfig{Host: "h", User: "u", DBName: "d", SSLMode: "verify-everything"})
	if err == nil {
		t.Fatal("an unknown sslmode composed a DSN")
	}
	for _, want := range []string{"verify-everything", "OPSKEEPER_DB_SSLMODE", "disable", "skip-verify"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// The field is documented as coming from the Helm chart, so a bad value has
// to fail at Load rather than at the first query.
func TestLoadRefusesAnUnusableSSLModeInsteadOfStartingPlaintext(t *testing.T) {
	t.Setenv("OPSKEEPER_DB_HOST", "db.internal")
	t.Setenv("OPSKEEPER_DB_SSLMODE", "yes-please")
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted an sslmode the driver cannot use")
	}
}

func dsnParam(t *testing.T, dsn, key string) string {
	t.Helper()
	_, after, ok := strings.Cut(dsn, "?")
	if !ok {
		t.Fatalf("dsn %q has no parameters", dsn)
	}
	for _, kv := range strings.Split(after, "&") {
		k, v, _ := strings.Cut(kv, "=")
		if k == key {
			return v
		}
	}
	return ""
}
