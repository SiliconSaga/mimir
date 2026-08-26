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
		"forgejo":  `"forgejo"`,
		`ev"il`:    `"ev""il"`,
		`a""b`:     `"a""""b"`,
	}
	for in, want := range cases {
		if got := quoteIdentifier(in); got != want {
			t.Errorf("quoteIdentifier(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuoteLiteral(t *testing.T) {
	cases := map[string]string{
		"hunter2":     `'hunter2'`,
		"it's":        `'it''s'`,
		"'; DROP --":  `'''; DROP --'`,
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
