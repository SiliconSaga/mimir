package engine

import "testing"

// These cover the string handling that reaches DDL. Identifiers cannot be
// parameterised in SQL, so they are interpolated — which makes validation and
// quoting the security boundary rather than a formatting detail.

func TestValidateIdentifier(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		valid bool
	}{
		{"simple", "forgejo", true},
		{"underscores", "my_app_db", true},
		{"leading underscore", "_internal", true},
		{"digits after first", "app2", true},

		{"empty", "", false},
		{"leading digit", "2fast", false},
		{"uppercase", "Forgejo", false},
		{"hyphen", "my-app", false},
		{"space", "my app", false},
		{"quote", `ev"il`, false},
		{"semicolon", "a;DROP DATABASE postgres", false},
		{"comment", "a--", false},
		{"unicode", "café", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateIdentifier(tc.in)
			if tc.valid && err != nil {
				t.Fatalf("expected %q to be valid, got %v", tc.in, err)
			}
			if !tc.valid && err == nil {
				t.Fatalf("expected %q to be rejected", tc.in)
			}
		})
	}
}

func TestValidateIdentifierLength(t *testing.T) {
	long := make([]byte, 64)
	for i := range long {
		long[i] = 'a'
	}
	if err := ValidateIdentifier(string(long)); err == nil {
		t.Fatal("expected a 64-character identifier to be rejected")
	}

	ok := long[:63]
	if err := ValidateIdentifier(string(ok)); err != nil {
		t.Fatalf("expected a 63-character identifier to be accepted, got %v", err)
	}
}

func TestQuoteIdentifier(t *testing.T) {
	cases := map[string]string{
		"forgejo": `"forgejo"`,
		`ev"il`:   `"ev""il"`,
		`a""b`:    `"a""""b"`,
	}
	for in, want := range cases {
		if got := quoteIdentifier(in); got != want {
			t.Errorf("quoteIdentifier(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuoteLiteral(t *testing.T) {
	cases := map[string]string{
		"hunter2":    `'hunter2'`,
		"it's":       `'it''s'`,
		"'; DROP --": `'''; DROP --'`,
	}
	for in, want := range cases {
		if got := quoteLiteral(in); got != want {
			t.Errorf("quoteLiteral(%q) = %q, want %q", in, got, want)
		}
	}
}

// A generated password must be URI-safe, because it is embedded in the
// connection string handed to consumers. base64url has no +/= to escape.
func TestGeneratePasswordIsURISafe(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		pw, err := generatePassword()
		if err != nil {
			t.Fatalf("generatePassword: %v", err)
		}
		if len(pw) < 32 {
			t.Fatalf("password too short: %d chars", len(pw))
		}
		if seen[pw] {
			t.Fatalf("generatePassword returned a duplicate: %q", pw)
		}
		seen[pw] = true

		for _, r := range pw {
			ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '-' || r == '_'
			if !ok {
				t.Fatalf("password %q contains a character needing URI escaping: %q", pw, r)
			}
		}
	}
}

// The admin password must never appear in an error, because errors land in log
// lines and status conditions that are far more widely readable than a Secret.
func TestConnectErrorOmitsPassword(t *testing.T) {
	tgt := Target{
		AdminHost:     "127.0.0.1",
		AdminPort:     1, // nothing listens here, so Connect fails fast
		AdminUser:     "postgres",
		AdminPassword: "super-secret-do-not-log",
		AdminDatabase: "postgres",
	}

	_, err := Postgres{}.connect(t.Context(), tgt, "postgres")
	if err == nil {
		t.Fatal("expected a connection error against port 1")
	}
	if contains(err.Error(), tgt.AdminPassword) {
		t.Fatalf("connection error leaked the admin password: %v", err)
	}
}

func contains(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestPostgresURIEscapesCredentials pins the escaping of the published URI.
//
// This string goes into a Secret and gets pasted into consumer config, so a
// credential that survives the round trip only for the characters our own
// generator happens to emit is not good enough. Generated tenant passwords are
// base64url, which is exactly why this needs a test rather than a live check:
// nothing we produce can reach the broken cases, so the bug stays invisible
// until somebody supplies a password by hand.
func TestPostgresURIEscapesCredentials(t *testing.T) {
	tgt := Target{Host: "pg.example.svc", Port: 5432}

	cases := []struct {
		name     string
		user     string
		password string
		want     string
	}{
		{
			// The regression that started this. QueryEscape encodes a space as
			// `+`, which means a space in a query string and a literal plus in
			// userinfo — so the consumer would authenticate with the wrong
			// password and get a bare "password authentication failed" with
			// nothing pointing at the encoding.
			name: "space becomes %20 rather than a plus",
			user: "app", password: "two words",
			want: "postgres://app:two%20words@pg.example.svc:5432/db?sslmode=disable",
		},
		{
			// The complement: a real plus has to stay a plus. Encoding a space
			// correctly is worth nothing if it costs the literal.
			name: "literal plus survives",
			user: "app", password: "a+b",
			want: "postgres://app:a+b@pg.example.svc:5432/db?sslmode=disable",
		},
		{
			// The failure that surfaced this area at all — the live admin
			// password contains a colon, which unescaped turns
			// `user:pass@host:port` into a parse error that quotes a fragment
			// of the password back at you.
			name: "colon cannot open a port",
			user: "app", password: "pa:ss",
			want: "postgres://app:pa%3Ass@pg.example.svc:5432/db?sslmode=disable",
		},
		{
			// An unescaped @ ends userinfo early, so the rest of the password
			// is read as the host.
			name: "at sign cannot end userinfo",
			user: "app", password: "p@ss",
			want: "postgres://app:p%40ss@pg.example.svc:5432/db?sslmode=disable",
		},
		{
			name: "the user half is escaped too",
			user: "a b", password: "pw",
			want: "postgres://a%20b:pw@pg.example.svc:5432/db?sslmode=disable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := postgresURI(tgt, "db", tc.user, tc.password); got != tc.want {
				t.Errorf("postgresURI() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPostgresURIBracketsIPv6Host guards the other half of the authority.
//
// An IPv6 literal's own colons are read as the port separator without brackets.
// The host is whatever the platform is configured with rather than something
// this code chooses, so it has to survive that.
func TestPostgresURIBracketsIPv6Host(t *testing.T) {
	tgt := Target{Host: "fd00::1", Port: 5432}
	want := "postgres://app:pw@[fd00::1]:5432/db?sslmode=disable"
	if got := postgresURI(tgt, "db", "app", "pw"); got != want {
		t.Errorf("postgresURI() = %q, want %q", got, want)
	}
}
