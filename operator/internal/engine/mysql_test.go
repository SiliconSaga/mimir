package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestGrantDatabasePatternEscapesWildcards is the most important test in this
// file. In a GRANT the database part is a LIKE pattern, so an unescaped
// underscore silently widens the grant across tenants — and DerivePhysicalName
// puts an underscore in almost every name it generates.
func TestGrantDatabasePatternEscapesWildcards(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"underscore is escaped", "app_one", "`app\\_one`"},
		{"percent is escaped", "app%one", "`app\\%one`"},
		{"backslash is escaped first", `app\one`, "`app\\\\one`"},
		{"plain name is unchanged apart from quoting", "appone", "`appone`"},
		{"several wildcards", "a_b_c", "`a\\_b\\_c`"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := grantDatabasePattern(tc.in); got != tc.want {
				t.Errorf("grantDatabasePattern(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestGrantDatabasePatternIsAlwaysBacktickQuoted guards the other half: escaping
// the wildcards is useless if the result is not also quoted, since the escape
// characters would then be parsed rather than applied.
func TestGrantDatabasePatternIsAlwaysBacktickQuoted(t *testing.T) {
	for _, in := range []string{"app", "app_one", "a", "z9_z"} {
		got := grantDatabasePattern(in)
		if !strings.HasPrefix(got, "`") || !strings.HasSuffix(got, "`") {
			t.Errorf("grantDatabasePattern(%q) = %q, want backtick-quoted", in, got)
		}
	}
}

// TestMySQLUserNameFitsLimit covers the constraint that makes MySQL identifiers
// different from Postgres's: user names cap at 32 while databases get 64, so a
// name the API considers valid can still be too long for CREATE USER.
func TestMySQLUserNameFitsLimit(t *testing.T) {
	// 63 characters — the longest DerivePhysicalName will produce, and legal
	// for a database.
	long := strings.Repeat("a", 63)

	got := mysqlUserName(long)
	if len(got) > mysqlMaxUserLength {
		t.Fatalf("mysqlUserName(63 chars) = %q (%d chars), want <= %d", got, len(got), mysqlMaxUserLength)
	}
}

func TestMySQLUserNameUnchangedWhenItFits(t *testing.T) {
	// The common case must stay recognisable: a short database name should give
	// a user of the same name, matching Postgres's role == database property.
	for _, in := range []string{"app", "team_a_app", strings.Repeat("b", mysqlMaxUserLength)} {
		if got := mysqlUserName(in); got != in {
			t.Errorf("mysqlUserName(%q) = %q, want it unchanged", in, got)
		}
	}
}

// TestMySQLUserNameDistinguishesLongNames is the reason truncation carries a
// hash. Two databases sharing a 32-character prefix must not collapse onto one
// account — that would hand both tenants the same credential and the same
// grants, which is the cross-tenant collision the derivation exists to prevent.
func TestMySQLUserNameDistinguishesLongNames(t *testing.T) {
	a := strings.Repeat("a", 60) + "one"
	b := strings.Repeat("a", 60) + "two"

	ua, ub := mysqlUserName(a), mysqlUserName(b)
	if ua == ub {
		t.Fatalf("mysqlUserName collided: %q and %q both gave %q", a, b, ua)
	}
	if len(ua) > mysqlMaxUserLength || len(ub) > mysqlMaxUserLength {
		t.Fatalf("derived users exceed the limit: %q (%d), %q (%d)", ua, len(ua), ub, len(ub))
	}
}

func TestMySQLUserNameIsDeterministic(t *testing.T) {
	in := strings.Repeat("c", 63)
	if mysqlUserName(in) != mysqlUserName(in) {
		t.Error("mysqlUserName is not deterministic; the account would move between reconciles")
	}
}

func TestQuoteMySQLIdentifier(t *testing.T) {
	cases := map[string]string{
		"app":     "`app`",
		"app_one": "`app_one`",
		// Doubling a backtick is what stops an identifier ending the quoted
		// region early. ValidateIdentifier makes this unreachable today.
		"we`ird": "`we``ird`",
	}
	for in, want := range cases {
		if got := quoteMySQLIdentifier(in); got != want {
			t.Errorf("quoteMySQLIdentifier(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestQuoteMySQLLiteralEscapesBackslash is the difference from the Postgres
// quoter. MySQL treats backslash as an escape inside string literals, so
// doubling quotes alone leaves a value able to change what follows it.
func TestQuoteMySQLLiteralEscapesBackslash(t *testing.T) {
	cases := map[string]string{
		"plain":  "'plain'",
		"it's":   "'it''s'",
		`back\ `: `'back\\ '`,
		// A classic escape-the-quote payload: without backslash handling the
		// trailing quote would be escaped by the backslash and the literal
		// would run on into the rest of the statement.
		`x\'`: `'x\\'''`,
	}
	for in, want := range cases {
		if got := quoteMySQLLiteral(in); got != want {
			t.Errorf("quoteMySQLLiteral(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOwnerAttributeJSONIsValidJSON(t *testing.T) {
	// Owners carry a UID and two Kubernetes names; the attribute has to survive
	// them intact or the marker comparison silently never matches.
	owner := "team-a/app/6f1b2c3d-4e5f-6789-abcd-ef0123456789"

	var decoded map[string]string
	if err := json.Unmarshal([]byte(ownerAttributeJSON(owner)), &decoded); err != nil {
		t.Fatalf("ownerAttributeJSON produced invalid JSON: %v", err)
	}
	if decoded[ownerAttributeKey] != owner {
		t.Errorf("owner round-trip = %q, want %q", decoded[ownerAttributeKey], owner)
	}
}

func TestOwnerAttributeJSONEscapesQuotes(t *testing.T) {
	// Not reachable from a real Owner(), which is built from Kubernetes names
	// and a UID. Tested anyway because the value is interpolated into SQL, and
	// "cannot happen" is the assumption that ages worst.
	owner := `a"b\c`

	var decoded map[string]string
	if err := json.Unmarshal([]byte(ownerAttributeJSON(owner)), &decoded); err != nil {
		t.Fatalf("ownerAttributeJSON produced invalid JSON for %q: %v", owner, err)
	}
	if decoded[ownerAttributeKey] != owner {
		t.Errorf("owner round-trip = %q, want %q", decoded[ownerAttributeKey], owner)
	}
}

func TestMySQLEngineIsMySQL(t *testing.T) {
	if got := (MySQL{}).Engine(); string(got) != "mysql" {
		t.Errorf("MySQL{}.Engine() = %q, want %q", got, "mysql")
	}
}

// TestMySQLURIIncludesTLSWhenRequired keeps the published URI honest: consumers
// paste it into their own config, and a URI that omits TLS against a server
// that requires it fails for them rather than here.
func TestMySQLURIIncludesTLSWhenRequired(t *testing.T) {
	tls := mysqlURI(Target{Host: "db.example", Port: 3306, TLS: true}, "app", "app", "pw")
	if !strings.Contains(tls, "tls=skip-verify") {
		t.Errorf("URI %q should carry a tls parameter", tls)
	}

	plain := mysqlURI(Target{Host: "db.example", Port: 3306}, "app", "app", "pw")
	if strings.Contains(plain, "tls=") {
		t.Errorf("URI %q should not carry a tls parameter", plain)
	}
}

// TestMySQLURIEscapesCredentials — generated passwords are base64url and safe,
// but a password reused from an existing Secret is whatever someone put there.
func TestMySQLURIEscapesCredentials(t *testing.T) {
	uri := mysqlURI(Target{Host: "db.example", Port: 3306}, "app", "user name", "p@ss:w/rd?")
	if strings.Contains(uri, "p@ss:w/rd?") {
		t.Errorf("URI %q contains an unescaped password", uri)
	}
	if strings.Contains(uri, "user name") {
		t.Errorf("URI %q contains an unescaped username", uri)
	}
}

// TestMySQLURIBracketsIPv6 mirrors the reasoning behind hostPort: without
// brackets an IPv6 literal's own colons read as the port separator.
func TestMySQLURIBracketsIPv6(t *testing.T) {
	uri := mysqlURI(Target{Host: "2001:db8::1", Port: 3306}, "app", "app", "pw")
	if !strings.Contains(uri, "[2001:db8::1]:3306") {
		t.Errorf("URI %q should bracket the IPv6 host", uri)
	}
}
