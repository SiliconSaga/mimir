package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/go-sql-driver/mysql"

	mimirv1alpha1 "github.com/SiliconSaga/mimir/operator/api/v1alpha1"
)

// MySQL vends a database + owning user inside an existing MySQL server.
//
// Written against Percona XtraDB Cluster, which is MySQL 8.0 plus Galera. The
// SQL here is plain MySQL 8.0 — nothing depends on Galera — but two Galera
// facts shaped it: DDL replicates cluster-wide via Total Order Isolation, so
// every statement below is a cluster-wide event rather than a local one, and
// that is a reason to keep the reconcile's steady state genuinely no-op rather
// than re-running harmless-looking DDL on a timer.
type MySQL struct{}

func (MySQL) Engine() mimirv1alpha1.Engine { return mimirv1alpha1.EngineMySQL }

// mysqlMaxUserLength is MySQL's hard limit on user names.
//
// This is the constraint that makes MySQL's identifiers different from
// Postgres's rather than merely similar. Database names get 64 characters, so
// the shared maxIdentifier of 63 covers them — the API's own comment says as
// much. But `mysql.user.User` is CHAR(32), and a 63-character name produces
// ERROR 1470 at CREATE USER, after the database has already been created.
//
// So unlike Postgres, where role and database are always the same string, the
// MySQL user is derived and may differ from the database name. Consumers read
// it from the published Secret, which is why they can.
const mysqlMaxUserLength = 32

// Ensure creates the database and its owning user if absent, sets the password,
// and grants that user rights on that database and nothing else.
//
// Deliberately NOT a transliteration of the Postgres provisioner. The isolation
// problem is the opposite shape: PostgreSQL lets any role connect to any
// database until you REVOKE CONNECT ... FROM PUBLIC, whereas MySQL grants
// nothing by default and there is no PUBLIC to revoke from. Porting the revoke
// would be cargo cult.
//
// MySQL's equivalent hazard is in the GRANT itself — see grantDatabasePattern.
func (m MySQL) Ensure(ctx context.Context, t Target, database, current string, opts Options) (Credentials, error) {
	var creds Credentials

	if err := ValidateIdentifier(database); err != nil {
		return creds, err
	}
	if database == ownerSchema {
		return creds, errReservedDatabase(database)
	}
	user := mysqlUserName(database)

	password := current
	if password == "" {
		var err error
		if password, err = generatePassword(); err != nil {
			return creds, fmt.Errorf("generate password: %w", err)
		}
	}

	db, err := m.connect(ctx, t, t.AdminDatabase)
	if err != nil {
		return creds, err
	}
	defer func() { _ = db.Close() }()

	qDB := quoteMySQLIdentifier(database)

	if err := m.ensureOwnerRegister(ctx, db); err != nil {
		return creds, err
	}

	// Ownership is checked BEFORE anything is mutated, for the same reason as
	// Postgres: discovering the conflict after ALTER USER would have rotated a
	// live tenant's password on the way to reporting it.
	dbExists, err := m.databaseExists(ctx, db, database)
	if err != nil {
		return creds, err
	}

	if opts.Owner != "" {
		owner, err := m.databaseOwnerMarker(ctx, db, database)
		if err != nil {
			return creds, err
		}

		switch {
		case owner == opts.Owner:
			// Ours. Carry on.

		case owner != "" && !dbExists:
			// A record with no database behind it. Either the database was
			// dropped by hand, or a previous run died between recording
			// ownership and creating it. Neither leaves data to protect, so the
			// name is free — refusing here would strand it permanently, needing
			// a human to clear a row nobody can see.

		case owner != "":
			return creds, &ErrNotOwned{Database: database, Want: opts.Owner, Got: owner}

		case dbExists:
			// No record, but the database is there — so it was created outside
			// the operator, or by something predating the register. Adopting it
			// would hand this tenant another's data and publish a password for
			// it, which is exactly what the marker exists to prevent.
			//
			// Note this is now the ONLY unmarked case, because ownership is
			// recorded before the database is created rather than after. The
			// earlier tenant-side marker had a crash window between the two and
			// needed the user's own marker as a fallback proof; moving the
			// register admin-side closed the window instead of working around it.
			return creds, &ErrNotOwned{Database: database, Want: opts.Owner, Got: ""}
		}

		// Recorded BEFORE the database is created. The reverse order is what
		// produced the crash window described above.
		if err := m.writeOwnerMarker(ctx, db, database, opts.Owner); err != nil {
			return creds, err
		}
	}

	// User first: it carries the marker that makes an unmarked database
	// adoptable above, so it has to exist before the database does.
	//
	// CREATE USER IF NOT EXISTS silently does nothing when the user is already
	// there — including not setting the password — so existence is checked
	// explicitly and the password is set separately on every pass. That keeps
	// the Secret and the server from drifting: rotate the Secret and the next
	// reconcile makes the server agree.
	userExists, err := m.userExists(ctx, db, user)
	if err != nil {
		return creds, err
	}
	if userExists && opts.Owner != "" {
		// Same reasoning as Postgres roles, and the stakes are identical:
		// adopting a hand-made user by name hands this tenant whatever that
		// user already holds — global GRANTs, access to other schemas — and
		// then publishes a working password for it in a Secret.
		marker, err := m.userOwnerMarker(ctx, db, user)
		if err != nil {
			return creds, err
		}
		if marker != opts.Owner {
			// Reported against the DATABASE, with the account named separately.
			// ErrNotOwned's message reads "database %q belongs to …", and it
			// lands verbatim in status.conditions — so naming the account there
			// would tell the user their database conflict is about a string
			// they never wrote. Postgres can get away with passing the role,
			// because there role and database are always the same name; here a
			// long database yields a truncated-and-hashed account nobody has
			// seen. Wrapped rather than replaced so errors.As still classifies
			// it as a Conflict.
			return creds, fmt.Errorf("its MySQL account %q is not ours: %w",
				user, &ErrNotOwned{Database: database, Want: opts.Owner, Got: marker})
		}
	}

	if !userExists {
		// '%' rather than a host pattern: consumers are pods whose addresses
		// are neither stable nor knowable here. Isolation comes from the grant
		// being scoped to one database, not from the host part.
		stmt := fmt.Sprintf("CREATE USER %s@%s IDENTIFIED BY %s",
			quoteMySQLLiteral(user), quoteMySQLLiteral("%"), quoteMySQLLiteral(password))
		if opts.Owner != "" {
			stmt += " ATTRIBUTE " + quoteMySQLLiteral(ownerAttributeJSON(opts.Owner))
		}
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return creds, fmt.Errorf("create user %q: %w", user, err)
		}
	} else {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("ALTER USER %s@%s IDENTIFIED BY %s",
			quoteMySQLLiteral(user), quoteMySQLLiteral("%"), quoteMySQLLiteral(password)),
		); err != nil {
			return creds, fmt.Errorf("set password for user %q: %w", user, err)
		}
		if opts.Owner != "" {
			if _, err := db.ExecContext(ctx, fmt.Sprintf("ALTER USER %s@%s ATTRIBUTE %s",
				quoteMySQLLiteral(user), quoteMySQLLiteral("%"),
				quoteMySQLLiteral(ownerAttributeJSON(opts.Owner))),
			); err != nil {
				return creds, fmt.Errorf("record owner on user %q: %w", user, err)
			}
		}
	}

	if !dbExists {
		// utf8mb4 explicitly rather than inheriting the server default. A
		// shared cluster serves tenants nobody has met yet, and the older
		// 3-byte utf8 cannot store emoji or many CJK characters — a default
		// that silently truncates user content is not a default worth
		// inheriting. Collation is left to the server so the platform can move
		// with MySQL's own default.
		if _, err := db.ExecContext(ctx,
			fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s CHARACTER SET utf8mb4", qDB),
		); err != nil {
			return creds, fmt.Errorf("create database %q: %w", database, err)
		}
	}

	// The grant. Scoped to exactly one database, and never global — a global
	// GRANT would reach every other tenant on the shared cluster.
	//
	// grantDatabasePattern is where MySQL's real isolation hazard lives; see
	// its comment. This is the line the Postgres REVOKE has no analogue for.
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf("GRANT ALL PRIVILEGES ON %s.* TO %s@%s",
			grantDatabasePattern(database), quoteMySQLLiteral(user), quoteMySQLLiteral("%")),
	); err != nil {
		return creds, fmt.Errorf("grant on %q to %q: %w", database, user, err)
	}

	return Credentials{
		Host:     t.Host,
		Port:     t.Port,
		Database: database,
		Username: user,
		Password: password,
		URI:      mysqlURI(t, database, user, password),
	}, nil
}

// Drop removes the database and its user, but ONLY when owner matches the
// marker recorded at creation. Same contract as Postgres.Drop, and an empty
// owner skips the check.
func (m MySQL) Drop(ctx context.Context, t Target, database, owner string) error {
	if err := ValidateIdentifier(database); err != nil {
		return err
	}
	if database == ownerSchema {
		return errReservedDatabase(database)
	}
	user := mysqlUserName(database)

	db, err := m.connect(ctx, t, t.AdminDatabase)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := m.ensureOwnerRegister(ctx, db); err != nil {
		return err
	}

	if owner != "" {
		// Checked against the register unconditionally, not only when the
		// database still exists. The register is the authority now, and a
		// database that has already been dropped by hand still needs its row
		// cleared — otherwise the name looks taken forever.
		marker, err := m.databaseOwnerMarker(ctx, db, database)
		if err != nil {
			return err
		}
		if marker != "" && marker != owner {
			// Someone else's. Leave the database, the user and the record
			// alone — this is "nothing here is mine", not an error.
			return nil
		}
		if marker == "" {
			exists, err := m.databaseExists(ctx, db, database)
			if err != nil {
				return err
			}
			if exists {
				// Unrecorded but present: created outside the operator, so not
				// ours to destroy.
				return nil
			}
		}
	}

	// No WITH (FORCE) equivalent is needed, and none exists. PostgreSQL refuses
	// to drop a database with live sessions, which is why its provisioner has
	// to force; MySQL drops it and the next statement on an open connection
	// simply fails. The failure mode Postgres guards against — deletion hanging
	// on a finalizer because one idle client held the database open — cannot
	// happen here.
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf("DROP DATABASE IF EXISTS %s", quoteMySQLIdentifier(database)),
	); err != nil {
		return fmt.Errorf("drop database %q: %w", database, err)
	}

	// The user is checked separately because it can outlive its database.
	if owner != "" {
		marker, err := m.userOwnerMarker(ctx, db, user)
		if err != nil {
			return err
		}
		if marker != owner {
			return nil
		}
	}
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf("DROP USER IF EXISTS %s@%s", quoteMySQLLiteral(user), quoteMySQLLiteral("%")),
	); err != nil {
		return fmt.Errorf("drop user %q: %w", user, err)
	}

	// Last, so a failure anywhere above leaves the record in place and the next
	// attempt still knows the database was ours. Clearing it first would turn a
	// half-finished drop into an unrecorded database that nothing will clean up.
	return m.deleteOwnerMarker(ctx, db, database)
}

// The ownership register lives in a schema of the operator's own, NOT inside
// the tenant's database.
//
// Postgres can afford to put its marker on the object itself, and this code
// originally copied that with a `_mimir_ownership` table inside each tenant
// database. That was wrong, and specifically because of how MySQL grants work:
// `GRANT ALL PRIVILEGES ON <db>.*` reaches every table in the database,
// including the marker. A tenant could rewrite or drop their own ownership
// record — and the damaging direction is not the obvious one. Pointing the
// marker at somebody else makes `Ensure` refuse, which is merely a
// self-inflicted outage; but it also makes `Drop` return nil without deleting
// anything, so removing the DataService would leave the database and its data
// behind, orphaned and invisible to the operator.
//
// A table-level REVOKE cannot fix that. MySQL privileges are additive: the
// database-level grant still applies, so the tenant keeps the access.
//
// The cost of moving it out is the drift the original comment worried about —
// a marker can now outlive a hand-dropped database. That is handled explicitly
// where it is read, rather than avoided by leaving the marker writable.
const (
	ownerSchema = "mimir_dataservice"
	ownerTable  = "ownership"
)

// errReservedDatabase rejects a tenant asking for the operator's own schema.
//
// Not hypothetical: `ValidateIdentifier` happily accepts "mimir_dataservice",
// and a DataService in namespace `mimir` named `dataservice` derives exactly
// that. Provisioning it would hand that tenant `GRANT ALL` over the ownership
// register for every other tenant on the cluster.
func errReservedDatabase(database string) error {
	return fmt.Errorf("database %q is reserved for the operator's own ownership register", database)
}

// ensureOwnerRegister creates the operator's own schema and register table.
//
// Idempotent and cheap, and run before the first read rather than only before a
// write: a fresh cluster has no register at all, and a read that treated
// "schema missing" as an error would fail every first reconcile.
func (m MySQL) ensureOwnerRegister(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, fmt.Sprintf(
		"CREATE DATABASE IF NOT EXISTS %s CHARACTER SET utf8mb4", quoteMySQLIdentifier(ownerSchema)),
	); err != nil {
		return fmt.Errorf("create owner register schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s.%s (
			database_name VARCHAR(64) NOT NULL PRIMARY KEY,
			owner VARCHAR(512) NOT NULL
		) ENGINE=InnoDB`,
		quoteMySQLIdentifier(ownerSchema), quoteMySQLIdentifier(ownerTable)),
	); err != nil {
		return fmt.Errorf("create owner register table: %w", err)
	}
	return nil
}

// writeOwnerMarker records ownership of a database in the operator's register.
//
// Runs on the ADMIN connection, so no second connection to the tenant database
// is needed — and, more to the point, the tenant has no grant that reaches this
// row. Keyed by database name and upserted, so a reconcile is a no-op.
func (m MySQL) writeOwnerMarker(ctx context.Context, db *sql.DB, database, owner string) error {
	if _, err := db.ExecContext(ctx, fmt.Sprintf(
		"INSERT INTO %s.%s (database_name, owner) VALUES (?, ?) ON DUPLICATE KEY UPDATE owner = VALUES(owner)",
		quoteMySQLIdentifier(ownerSchema), quoteMySQLIdentifier(ownerTable)), database, owner,
	); err != nil {
		return fmt.Errorf("record owner of %q: %w", database, err)
	}
	return nil
}

// deleteOwnerMarker removes a database's row from the register.
//
// Called from Drop, and it must be: the register outlives the database, so a
// row left behind would make a later request for the same name look like
// someone else's database rather than a free one.
func (m MySQL) deleteOwnerMarker(ctx context.Context, db *sql.DB, database string) error {
	if _, err := db.ExecContext(ctx, fmt.Sprintf(
		"DELETE FROM %s.%s WHERE database_name = ?",
		quoteMySQLIdentifier(ownerSchema), quoteMySQLIdentifier(ownerTable)), database,
	); err != nil {
		return fmt.Errorf("clear owner record for %q: %w", database, err)
	}
	return nil
}

// databaseOwnerMarker reads the owner recorded for a database, or "" when the
// register has no row — meaning the operator did not create it.
func (m MySQL) databaseOwnerMarker(ctx context.Context, db *sql.DB, database string) (string, error) {
	var owner string
	err := db.QueryRowContext(ctx, fmt.Sprintf(
		"SELECT owner FROM %s.%s WHERE database_name = ?",
		quoteMySQLIdentifier(ownerSchema), quoteMySQLIdentifier(ownerTable)), database,
	).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read owner record for %q: %w", database, err)
	}
	return owner, nil
}

// userOwnerMarker reads the owner recorded on a user, or "" when unmarked.
//
// MySQL 8.0.21+ attaches arbitrary JSON to an account via CREATE/ALTER USER ...
// ATTRIBUTE, surfaced through INFORMATION_SCHEMA.USER_ATTRIBUTES. That is the
// closest thing MySQL has to COMMENT ON ROLE, and it has the property that
// matters: it lives on the account, so it cannot drift and it vanishes with it.
func (m MySQL) userOwnerMarker(ctx context.Context, db *sql.DB, user string) (string, error) {
	var attrs sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT ATTRIBUTE FROM information_schema.user_attributes
		  WHERE USER = ? AND HOST = '%'`, user,
	).Scan(&attrs)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read owner attribute on user %q: %w", user, err)
	}
	if !attrs.Valid || attrs.String == "" {
		return "", nil
	}

	// Read back with JSON_EXTRACT rather than parsing here, so the key and the
	// escaping stay owned by the same engine that wrote them.
	var owner sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT JSON_UNQUOTE(JSON_EXTRACT(ATTRIBUTE, '$.mimirOwner'))
		   FROM information_schema.user_attributes WHERE USER = ? AND HOST = '%'`, user,
	).Scan(&owner); err != nil {
		return "", fmt.Errorf("read owner attribute on user %q: %w", user, err)
	}
	if !owner.Valid {
		return "", nil
	}
	return owner.String, nil
}

func (m MySQL) databaseExists(ctx context.Context, db *sql.DB, database string) (bool, error) {
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = ?`, database,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("check database %q: %w", database, err)
	}
	return count > 0, nil
}

func (m MySQL) userExists(ctx context.Context, db *sql.DB, user string) (bool, error) {
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mysql.user WHERE User = ? AND Host = '%'`, user,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("check user %q: %w", user, err)
	}
	return count > 0, nil
}

// connect opens an admin connection.
//
// Unlike Postgres there is no pooler caveat: MySQL DDL is not transactional, so
// nothing here needs the primary specifically. AdminHost is still honoured
// because the Target contract offers it and a deployment may route admin
// traffic separately.
func (MySQL) connect(ctx context.Context, t Target, database string) (*sql.DB, error) {
	cfg := mysql.NewConfig()
	cfg.User = t.AdminUser
	cfg.Passwd = t.AdminPassword
	cfg.Net = "tcp"
	cfg.Addr = hostPort(t.AdminHost, t.AdminPort)
	cfg.DBName = database
	// Interpolation off: placeholders go to the server as placeholders, which
	// is what keeps the parameterised queries above genuinely parameterised.
	cfg.InterpolateParams = false
	cfg.ParseTime = true
	if t.TLS {
		// skip-verify, matching the Postgres provisioner's sslmode=require: the
		// server certificate is issued by the operator's own internal CA, which
		// this client does not carry. Encrypted transport, no identity check —
		// and the same hardening follow-up applies to both.
		cfg.TLSConfig = "skip-verify"
	}

	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		// The DSN carries the admin password, so err is never wrapped with it.
		return nil, fmt.Errorf("open connection to %s:%d/%s as %s: %w",
			t.AdminHost, t.AdminPort, database, t.AdminUser, err)
	}

	// database/sql opens lazily, so without this a bad address surfaces at the
	// first query as a confusing mid-reconcile failure rather than here.
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to %s:%d/%s as %s: %w",
			t.AdminHost, t.AdminPort, database, t.AdminUser, err)
	}
	return db, nil
}

// mysqlUserName derives an account name that fits MySQL's 32-character limit.
//
// Returns the database name unchanged when it already fits, which is the normal
// case and keeps the Postgres-like property that user and database usually
// match. Longer names are truncated with a hash of the FULL name appended — the
// same technique, and for the same reason, as DerivePhysicalName: plain
// truncation would map two long distinct databases onto one shared account,
// which is precisely the cross-tenant collision the derivation exists to avoid.
func mysqlUserName(database string) string {
	if len(database) <= mysqlMaxUserLength {
		return database
	}
	sum := sha256.Sum256([]byte(database))
	suffix := "_" + hex.EncodeToString(sum[:])[:8]
	return database[:mysqlMaxUserLength-len(suffix)] + suffix
}

// grantDatabasePattern renders a database name for the `ON <db>.*` part of a
// GRANT, escaping LIKE metacharacters.
//
// ★ This is the MySQL isolation bug that has no Postgres counterpart. In a
// GRANT, the database part is a PATTERN, not an identifier: `_` matches any
// single character and `%` matches any sequence. Underscores are legal in our
// identifiers and common in derived names, since DerivePhysicalName joins
// namespace and object with one.
//
// So `GRANT ALL ON app_one.* TO ...` also grants that tenant full rights on
// `appXone`, `app1one`, and every other database matching the pattern —
// including databases belonging to other tenants that do not exist yet. Nothing
// errors, nothing logs, and the extra privileges appear the moment a matching
// database is created later.
//
// Escaping with backslashes makes the pattern literal. The identifier is still
// backtick-quoted afterwards, because escaping and quoting solve different
// problems: this one stops pattern matching, the quotes stop parsing.
func grantDatabasePattern(database string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `_`, `\_`, `%`, `\%`).Replace(database)
	return "`" + strings.ReplaceAll(escaped, "`", "``") + "`"
}

// ownerAttributeKey is the JSON key holding the owner inside the user
// attribute. Namespaced enough that an attribute set by something else does not
// collide, mirroring ownerMarkerPrefix's job on the Postgres side.
const ownerAttributeKey = "mimirOwner"

// ownerAttributeJSON builds the JSON document stored in the user attribute.
//
// Built with encoding/json rather than string concatenation: the owner carries
// a UID and two Kubernetes names, and hand-rolling JSON around values that came
// from the API server is how a malformed — or hostile — attribute gets in.
func ownerAttributeJSON(owner string) string {
	b, err := json.Marshal(map[string]string{ownerAttributeKey: owner})
	if err != nil {
		// Marshalling a map of two strings cannot fail. If it somehow does, an
		// empty attribute is safer than a malformed one: the marker comparison
		// then reports a mismatch and refuses, rather than writing SQL built
		// from a half-encoded string.
		return "{}"
	}
	return string(b)
}

// mysqlURI assembles the convenience connection string published in the Secret.
//
// ⚠️ There is no standard MySQL URI, and the `tls` parameter below is
// specifically go-sql-driver/mysql's spelling. Connector/J calls it `sslMode`,
// Connector/Python takes `ssl_*` connection arguments and no URI parameter at
// all, and PHP's mysqli does not accept a URI in the first place. A non-Go
// consumer that pastes this string will therefore ignore the TLS instruction —
// and against a Percona cluster, which requires TLS, that fails to connect.
//
// The URI is a convenience, not the contract. The Secret also carries `host`,
// `port`, `database`, `username` and `password` as discrete keys, and those are
// what a non-Go consumer should use, configuring TLS in whatever way its own
// client spells it. Documented in the README next to the Secret contract.
//
// Kept rather than stripped: dropping the parameter would silently downgrade Go
// consumers, who are the ones actually able to use the string, to a plaintext
// attempt against a TLS-required server. Better to be right for the audience it
// serves and explicit about who that is.
func mysqlURI(t Target, database, user, password string) string {
	u := url.URL{
		Scheme: "mysql",
		User:   url.UserPassword(user, password),
		Host:   hostPort(t.Host, t.Port),
		Path:   "/" + database,
	}
	if t.TLS {
		q := u.Query()
		q.Set("tls", "skip-verify")
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// quoteMySQLIdentifier backtick-quotes an identifier, doubling embedded
// backticks. MySQL does not accept double quotes for identifiers unless the
// server runs in ANSI_QUOTES mode, which a shared cluster must not assume — so
// this cannot reuse the Postgres quoter.
func quoteMySQLIdentifier(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// quoteMySQLLiteral single-quotes a string literal.
//
// Escapes backslashes as well as quotes, which the Postgres version does not
// need to: MySQL treats backslash as an escape character inside string literals
// by default, so doubling quotes alone would let a value containing a backslash
// change the meaning of what follows. Generated passwords are base64url and
// contain neither, but user names and owners reach here from the API server.
func quoteMySQLLiteral(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `''`)
	return `'` + r.Replace(s) + `'`
}
