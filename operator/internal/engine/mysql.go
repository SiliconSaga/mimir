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

	// Ownership is checked BEFORE anything is mutated, for the same reason as
	// Postgres: discovering the conflict after ALTER USER would have rotated a
	// live tenant's password on the way to reporting it.
	dbExists, err := m.databaseExists(ctx, db, database)
	if err != nil {
		return creds, err
	}
	if dbExists && opts.Owner != "" {
		owner, err := m.databaseOwnerMarker(ctx, db, database)
		if err != nil {
			return creds, err
		}
		if owner != opts.Owner {
			// An UNMARKED database may be our own interrupted work. MySQL DDL
			// does not roll back, so a crash between CREATE DATABASE and the
			// marker table leaves exactly this. The user is created and marked
			// first, so a database with no marker whose derived user carries
			// our marker can only be ours — the same narrow proof Postgres
			// uses, with user-attribute standing in for role comment.
			adoptable := false
			if owner == "" {
				userMarker, uerr := m.userOwnerMarker(ctx, db, user)
				if uerr != nil {
					return creds, uerr
				}
				adoptable = userMarker == opts.Owner
			}
			if !adoptable {
				return creds, &ErrNotOwned{Database: database, Want: opts.Owner, Got: owner}
			}
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
			return creds, &ErrNotOwned{Database: user, Want: opts.Owner, Got: marker}
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

	// Stamp ownership on the database itself. MySQL has no COMMENT ON DATABASE
	// — the one piece of Postgres's design with no direct equivalent — so the
	// marker is a table inside the tenant database. That keeps the property
	// that made the Postgres comment the right choice: it lives with the
	// database, so it cannot drift out of sync and it disappears exactly when
	// the database does. A side table in the admin schema would outlive a
	// hand-dropped database and then refuse to recreate it.
	if opts.Owner != "" {
		if err := m.writeOwnerMarker(ctx, t, database, opts.Owner); err != nil {
			return creds, err
		}
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
	user := mysqlUserName(database)

	db, err := m.connect(ctx, t, t.AdminDatabase)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if owner != "" {
		exists, err := m.databaseExists(ctx, db, database)
		if err != nil {
			return err
		}
		if exists {
			marker, err := m.databaseOwnerMarker(ctx, db, database)
			if err != nil {
				return err
			}
			if marker != owner {
				// Someone else's, or made by hand. Leave the database and the
				// user alone — this is "nothing here is mine", not an error.
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
	return nil
}

// ownerMarkerTable is the table holding the ownership marker, inside the
// tenant's own database. Leading underscore keeps it out of the way of
// application tables sorted by name.
const ownerMarkerTable = "_mimir_ownership"

// writeOwnerMarker records the owner in a table inside the tenant database.
//
// Connects to the tenant database rather than the admin one because that is
// where the table lives; MySQL has no cross-database DDL shortcut worth the
// string concatenation it would take.
func (m MySQL) writeOwnerMarker(ctx context.Context, t Target, database, owner string) error {
	conn, err := m.connect(ctx, t, database)
	if err != nil {
		return fmt.Errorf("connect to %q to record owner: %w", database, err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s (
			id TINYINT NOT NULL PRIMARY KEY,
			owner VARCHAR(512) NOT NULL
		) ENGINE=InnoDB`, quoteMySQLIdentifier(ownerMarkerTable)),
	); err != nil {
		return fmt.Errorf("create owner marker table in %q: %w", database, err)
	}

	// A single row, upserted. The fixed primary key is what makes it single —
	// without it a reconcile loop would append a row per pass.
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(
		"INSERT INTO %s (id, owner) VALUES (1, ?) ON DUPLICATE KEY UPDATE owner = VALUES(owner)",
		quoteMySQLIdentifier(ownerMarkerTable)), owner,
	); err != nil {
		return fmt.Errorf("record owner in %q: %w", database, err)
	}
	return nil
}

// databaseOwnerMarker reads the owner recorded inside a database, or "" when
// there is no marker — meaning it was created outside the operator.
func (m MySQL) databaseOwnerMarker(ctx context.Context, db *sql.DB, database string) (string, error) {
	// Checked via information_schema rather than by querying the table and
	// treating the error as absence. "Table doesn't exist" and "you cannot read
	// it" are different answers, and only the first one means unmarked.
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables
		  WHERE table_schema = ? AND table_name = ?`,
		database, ownerMarkerTable,
	).Scan(&count); err != nil {
		return "", fmt.Errorf("look for owner marker in %q: %w", database, err)
	}
	if count == 0 {
		return "", nil
	}

	var owner string
	err := db.QueryRowContext(ctx, fmt.Sprintf(
		"SELECT owner FROM %s.%s WHERE id = 1",
		quoteMySQLIdentifier(database), quoteMySQLIdentifier(ownerMarkerTable)),
	).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read owner marker in %q: %w", database, err)
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

func mysqlURI(t Target, database, user, password string) string {
	// The scheme MySQL clients and most ORMs expect. Distinct from the Go
	// driver's own DSN, which is not a URL — consumers get the portable form.
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
