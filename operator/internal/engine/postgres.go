package engine

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	mimirv1alpha1 "github.com/SiliconSaga/mimir/operator/api/v1alpha1"
)

// Postgres vends a database + owning role inside an existing PostgreSQL server.
type Postgres struct{}

func (Postgres) Engine() mimirv1alpha1.Engine { return mimirv1alpha1.EnginePostgres }

// Ensure creates the role and database if absent, fixes ownership and grants,
// and revokes PUBLIC's default ability to connect.
//
// That last part is not optional. PostgreSQL lets ANY role connect to ANY
// database by default, so without the revoke a shared cluster is shared data —
// every tenant could read every other tenant's database. It is the difference
// between multi-tenant and merely co-located.
func (p Postgres) Ensure(ctx context.Context, t Target, database, current string, opts Options) (Credentials, error) {
	var creds Credentials

	if err := ValidateIdentifier(database); err != nil {
		return creds, err
	}
	role := database

	password := current
	if password == "" {
		var err error
		if password, err = generatePassword(); err != nil {
			return creds, fmt.Errorf("generate password: %w", err)
		}
	}

	conn, err := p.connect(ctx, t, t.AdminDatabase)
	if err != nil {
		return creds, err
	}
	defer func() { _ = conn.Close(ctx) }()

	// Identifiers cannot be parameterised, so they are quoted rather than
	// interpolated raw; ValidateIdentifier above is the belt to this braces.
	qRole := quoteIdentifier(role)
	qDB := quoteIdentifier(database)

	// Ownership is checked BEFORE anything is mutated, and the order matters.
	// Setting the role password first — as an earlier version did — would
	// rotate another tenant's credential and only then discover the database
	// was not ours, breaking a working service on the way to reporting a
	// conflict.
	var dbExists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, database,
	).Scan(&dbExists); err != nil {
		return creds, fmt.Errorf("check database %q: %w", database, err)
	}
	if dbExists && opts.Owner != "" {
		owner, err := p.databaseOwnerMarker(ctx, conn, database)
		if err != nil {
			return creds, err
		}
		if owner != opts.Owner {
			// An UNMARKED database might be our own half-finished work rather
			// than a stranger's. CREATE DATABASE cannot run in a transaction,
			// so a crash between creating it and stamping the marker leaves
			// exactly this state — and rejecting it outright would wedge the
			// request permanently, with status never populated and therefore
			// nothing recorded for deletion to clean up either.
			//
			// The role is created and marked BEFORE the database, so a database
			// owned by a role bearing our marker can only have been created by
			// us. That is a narrow enough proof to adopt on: it does not let us
			// claim any database that merely happens to be unmarked.
			adoptable := false
			if owner == "" {
				roleMarker, rerr := p.roleOwnerMarker(ctx, conn, role)
				if rerr != nil {
					return creds, rerr
				}
				dbOwner, oerr := p.databaseOwningRole(ctx, conn, database)
				if oerr != nil {
					return creds, oerr
				}
				adoptable = roleMarker == opts.Owner && dbOwner == role
			}
			if !adoptable {
				return creds, &ErrNotOwned{Database: database, Want: opts.Owner, Got: owner}
			}
		}
	}

	// Role next: CREATE DATABASE ... OWNER requires the role to exist.
	//
	// CREATE ROLE has no IF NOT EXISTS, so existence is checked separately.
	// The password is set on every reconcile, not just at creation, so that the
	// Secret and the server cannot drift apart — if someone rotates the Secret,
	// the next reconcile makes the server agree.
	var roleExists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, role,
	).Scan(&roleExists); err != nil {
		return creds, fmt.Errorf("check role %q: %w", role, err)
	}

	if !roleExists {
		if _, err := conn.Exec(ctx,
			fmt.Sprintf("CREATE ROLE %s WITH LOGIN PASSWORD %s", qRole, quoteLiteral(password)),
		); err != nil {
			return creds, fmt.Errorf("create role %q: %w", role, err)
		}
	} else {
		// Roles carry an ownership marker too, and for a sharper reason than
		// databases. Adopting an existing role by name alone hands the new
		// tenant whatever that role already has — memberships, SUPERUSER,
		// ownership of other schemas — and ALTER ROLE ... PASSWORD publishes a
		// credential for it. A dangling or hand-made role would become a
		// privilege escalation delivered in a Secret.
		if opts.Owner != "" {
			marker, err := p.roleOwnerMarker(ctx, conn, role)
			if err != nil {
				return creds, err
			}
			if marker != opts.Owner {
				return creds, &ErrNotOwned{Database: role, Want: opts.Owner, Got: marker}
			}
		}
		if _, err := conn.Exec(ctx,
			fmt.Sprintf("ALTER ROLE %s WITH LOGIN PASSWORD %s", qRole, quoteLiteral(password)),
		); err != nil {
			return creds, fmt.Errorf("set password for role %q: %w", role, err)
		}
	}

	if opts.Owner != "" {
		if _, err := conn.Exec(ctx,
			fmt.Sprintf("COMMENT ON ROLE %s IS %s", qRole, quoteLiteral(ownerMarkerPrefix+opts.Owner)),
		); err != nil {
			return creds, fmt.Errorf("record owner on role %q: %w", role, err)
		}
	}

	if !dbExists {
		// ALLOW_CONNECTIONS false closes the window between creation and the
		// REVOKE below. A new database accepts connections from every role on
		// the cluster by default, so without this it is briefly readable by
		// every other tenant — and if the process dies in that gap, it stays
		// that way until a later reconcile happens to succeed. Connections are
		// enabled again only once the grants are correct.
		if _, err := conn.Exec(ctx,
			fmt.Sprintf("CREATE DATABASE %s OWNER %s ALLOW_CONNECTIONS false", qDB, qRole),
		); err != nil {
			return creds, fmt.Errorf("create database %q: %w", database, err)
		}
	}

	// Reassert the owning role on every pass, not just at creation. The API
	// promises an owning role, and an administrator reassigning ownership by
	// hand would otherwise cost the tenant owner-only rights — including
	// public-schema DDL under PostgreSQL 15 — while this still reported Ready.
	if _, err := conn.Exec(ctx,
		fmt.Sprintf("ALTER DATABASE %s OWNER TO %s", qDB, qRole),
	); err != nil {
		return creds, fmt.Errorf("set owner of %q to %q: %w", database, role, err)
	}

	// Stamp ownership. COMMENT ON DATABASE is used rather than a side table
	// because it lives with the database itself — it cannot drift out of sync,
	// and it survives anything short of dropping the database.
	if opts.Owner != "" {
		if _, err := conn.Exec(ctx,
			fmt.Sprintf("COMMENT ON DATABASE %s IS %s", qDB, quoteLiteral(ownerMarkerPrefix+opts.Owner)),
		); err != nil {
			return creds, fmt.Errorf("record owner on %q: %w", database, err)
		}
	}

	// Tenant isolation. REVOKE ... FROM PUBLIC is what stops every other role
	// on the shared cluster from connecting to this database.
	if _, err := conn.Exec(ctx,
		fmt.Sprintf("REVOKE CONNECT ON DATABASE %s FROM PUBLIC", qDB),
	); err != nil {
		return creds, fmt.Errorf("revoke public connect on %q: %w", database, err)
	}
	if _, err := conn.Exec(ctx,
		fmt.Sprintf("GRANT ALL PRIVILEGES ON DATABASE %s TO %s", qDB, qRole),
	); err != nil {
		return creds, fmt.Errorf("grant on %q: %w", database, err)
	}

	// Grants are correct now, so the database can accept connections. Runs
	// every reconcile rather than only after creation, so a database left
	// closed by an interrupted earlier pass is repaired rather than stranded.
	if _, err := conn.Exec(ctx,
		fmt.Sprintf("ALTER DATABASE %s WITH ALLOW_CONNECTIONS true", qDB),
	); err != nil {
		return creds, fmt.Errorf("enable connections on %q: %w", database, err)
	}

	// Make the database reachable through the pooler, which is what the URI
	// below actually points at. Also after connections are enabled: the auth
	// objects live inside the tenant database.
	if err := p.ensurePoolerAuth(ctx, conn, t, database); err != nil {
		return creds, err
	}

	// Extensions come after connections are enabled — ensureExtensions has to
	// connect to the tenant database itself.
	if len(opts.Extensions) > 0 {
		if err := p.ensureExtensions(ctx, t, database, opts.Extensions); err != nil {
			return creds, err
		}
	}

	return Credentials{
		Host:     t.Host,
		Port:     t.Port,
		Database: database,
		Username: role,
		Password: password,
		URI:      postgresURI(t, database, role, password),
	}, nil
}

// ownerMarkerPrefix namespaces the comment so a database that merely happens
// to carry a human-written description is not mistaken for one of ours.
const ownerMarkerPrefix = "mimir-dataservice:"

// databaseOwnerMarker returns the owner recorded on a database, or "" when
// there is no marker — which means it was created outside the operator.
func (Postgres) databaseOwnerMarker(ctx context.Context, conn *pgx.Conn, database string) (string, error) {
	var comment *string
	if err := conn.QueryRow(ctx,
		`SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1`,
		database,
	).Scan(&comment); err != nil {
		return "", fmt.Errorf("read owner marker on %q: %w", database, err)
	}
	if comment == nil {
		return "", nil
	}
	if !strings.HasPrefix(*comment, ownerMarkerPrefix) {
		return "", nil
	}
	return strings.TrimPrefix(*comment, ownerMarkerPrefix), nil
}

// databaseOwningRole returns the name of the role that owns a database.
//
// Used only to decide whether an unmarked database is our own interrupted work,
// so it is deliberately a fact about the server rather than about our marker.
func (Postgres) databaseOwningRole(ctx context.Context, conn *pgx.Conn, database string) (string, error) {
	var owner string
	if err := conn.QueryRow(ctx,
		`SELECT r.rolname FROM pg_database d
		   JOIN pg_roles r ON r.oid = d.datdba
		  WHERE d.datname = $1`,
		database,
	).Scan(&owner); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("read owning role of %q: %w", database, err)
	}
	return owner, nil
}

// roleOwnerMarker returns the owner recorded on a role, or "" when there is no
// marker — meaning the role was created outside the operator.
func (Postgres) roleOwnerMarker(ctx context.Context, conn *pgx.Conn, role string) (string, error) {
	// FROM pg_roles, not pg_authid. pg_authid holds password hashes and is
	// readable by superusers only, so an admin role with CREATEROLE but not
	// SUPERUSER — a perfectly reasonable way to run this operator — gets a
	// permission error instead of a marker. Worse in Drop, which would have
	// removed the database and then failed reading the role, orphaning it.
	//
	// pg_roles is the public view over the same rows. The catalog argument to
	// shobj_description stays 'pg_authid', because that is where the comment is
	// recorded regardless of which view is queried.
	var comment *string
	if err := conn.QueryRow(ctx,
		`SELECT shobj_description(oid, 'pg_authid') FROM pg_roles WHERE rolname = $1`,
		role,
	).Scan(&comment); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("read owner marker on role %q: %w", role, err)
	}
	if comment == nil || !strings.HasPrefix(*comment, ownerMarkerPrefix) {
		return "", nil
	}
	return strings.TrimPrefix(*comment, ownerMarkerPrefix), nil
}

// poolerAuthSchema and poolerAuthFunction are the names pgBouncer's auth_query
// resolves. They are NOT configurable, because this operator does not write
// that query — the pooler's own deployment does, and it names these.
const (
	poolerAuthSchema   = "pgbouncer"
	poolerAuthFunction = "get_auth"
)

// getAuthDDL is the password-lookup function the pooler calls.
//
// SECURITY DEFINER is unavoidable: pg_authid holds the password verifiers and
// is superuser-only, while the caller is an unprivileged pooler role. That
// makes SET search_path mandatory rather than tidy. The function is installed
// in a database the TENANT owns, so without a pinned search_path the tenant
// could create their own pg_catalog-shadowing objects and have this function
// resolve to them while running with the admin role's rights — a clean
// privilege escalation out of their own database. The path is pinned and every
// reference inside the body is schema-qualified as well.
//
// getAuthResultType is what pg_get_function_result reports for the signature
// below, used to recognise a function of this name that is not ours. Asserted
// in the integration tests rather than trusted, since it is a rendering of the
// DDL by the server and not a string this package controls.
const getAuthResultType = "TABLE(username text, password text)"

// rolcanlogin filters out group roles, which have no password to hand back.
const getAuthDDL = `CREATE OR REPLACE FUNCTION ` + poolerAuthSchema + `.` + poolerAuthFunction + `(p_username TEXT)
  RETURNS TABLE (username TEXT, password TEXT)
  LANGUAGE sql
  SECURITY DEFINER
  SET search_path = pg_catalog
AS $$
  SELECT rolname::TEXT, rolpassword::TEXT
    FROM pg_catalog.pg_authid
   WHERE rolname = $1 AND rolcanlogin
$$`

// ensurePoolerAuth makes a vended database reachable THROUGH the pooler.
//
// Consumers are handed a URI pointing at the pooler — that is the entire reason
// a pooler is deployed. But a pooler does not check passwords itself: it
// connects as its own role into the database the client named and runs an
// auth_query there to fetch the stored verifier. Both halves of that are
// missing in a database this operator creates:
//
//   - CONNECT was revoked from PUBLIC, which is what makes the shared cluster
//     multi-tenant rather than merely co-located. That revoke catches the
//     pooler's role too.
//   - The lookup function only exists in databases the cluster operator made
//     itself, so ours do not have it.
//
// The failure this produced was quiet and actively misleading. pgBouncer
// reports the failed lookup to the client as `permission denied for database
// "x"` — character for character what a correctly refused CROSS-tenant attempt
// produces. So the published URI never worked, and the end-to-end isolation
// assertion passed for the wrong reason: it could not tell "tenant-a is
// properly locked out of tenant-b" from "nobody can reach anything".
//
// What is granted to the pooler role is deliberately narrow: CONNECT, USAGE on
// one schema, and EXECUTE on one function. It gets no rights over tenant data.
// It CAN read every login role's password verifier through that function —
// that is inherent to how auth_query works, and is equally true of the
// databases the cluster operator manages itself. The role is a cluster-level
// service identity, not a tenant, so this widens nothing that the pooler did
// not already have.
//
// An absent role means there is no pooler of this shape in front of the server
// — a direct-to-primary deployment, or a different pooler — so there is
// nothing to bootstrap and nothing wrong. Every other failure is returned:
// reporting Ready while handing out a URI that cannot connect is the bug this
// function exists to fix.
func (p Postgres) ensurePoolerAuth(ctx context.Context, conn *pgx.Conn, t Target, database string) error {
	authRole := t.PoolerAuthRole
	if authRole == "" {
		return nil
	}
	if err := ValidateIdentifier(authRole); err != nil {
		return fmt.Errorf("pooler auth role: %w", err)
	}

	var roleExists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, authRole,
	).Scan(&roleExists); err != nil {
		return fmt.Errorf("check pooler auth role %q: %w", authRole, err)
	}
	if !roleExists {
		return nil
	}

	qAuth := quoteIdentifier(authRole)
	if _, err := conn.Exec(ctx,
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", quoteIdentifier(database), qAuth),
	); err != nil {
		return fmt.Errorf("grant pooler connect on %q: %w", database, err)
	}

	// The rest is per-database, so it needs a connection to the database
	// itself rather than the admin one.
	dbConn, err := p.connect(ctx, t, database)
	if err != nil {
		return fmt.Errorf("connect to %q for pooler auth: %w", database, err)
	}
	defer func() { _ = dbConn.Close(ctx) }()

	qSchema := quoteIdentifier(poolerAuthSchema)
	qFunc := qSchema + "." + quoteIdentifier(poolerAuthFunction)
	// The tenant owns the database and can therefore CREATE in it, including a
	// schema of this name. Ownership is reasserted rather than assumed so a
	// tenant cannot own the schema that houses a SECURITY DEFINER function.
	qAdmin := quoteIdentifier(t.AdminUser)

	for _, stmt := range []string{
		fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", qSchema),
		fmt.Sprintf("ALTER SCHEMA %s OWNER TO %s", qSchema, qAdmin),
		fmt.Sprintf("REVOKE ALL ON SCHEMA %s FROM PUBLIC", qSchema),
		fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s", qSchema, qAuth),
	} {
		if _, err := dbConn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("bootstrap pooler auth in %q: %w", database, err)
		}
	}

	// Clear a function of this name that is not the one we are about to write.
	//
	// CREATE OR REPLACE cannot change a function's return type, and the tenant
	// owns this database, so a get_auth(TEXT) they declared differently would
	// make every later reconcile fail on the DDL below — permanently, with the
	// DataService never reaching Ready. It also cannot change the owner, so a
	// function they created would stay theirs and run as them.
	//
	// Conditional rather than an unconditional DROP-then-CREATE, because this
	// function is on the client login path: recreating it on every ten-minute
	// reconcile would put a window in front of every login, to fix a state
	// that is almost never present.
	stale, err := p.poolerAuthFunctionIsForeign(ctx, dbConn, t.AdminUser)
	if err != nil {
		return fmt.Errorf("inspect pooler auth function in %q: %w", database, err)
	}
	if stale {
		if _, err := dbConn.Exec(ctx,
			fmt.Sprintf("DROP FUNCTION IF EXISTS %s(TEXT)", qFunc),
		); err != nil {
			return fmt.Errorf("replace foreign pooler auth function in %q: %w", database, err)
		}
	}

	for _, stmt := range []string{
		getAuthDDL,
		fmt.Sprintf("ALTER FUNCTION %s(TEXT) OWNER TO %s", qFunc, qAdmin),
		fmt.Sprintf("REVOKE ALL ON FUNCTION %s(TEXT) FROM PUBLIC", qFunc),
		fmt.Sprintf("GRANT EXECUTE ON FUNCTION %s(TEXT) TO %s", qFunc, qAuth),
	} {
		if _, err := dbConn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("bootstrap pooler auth in %q: %w", database, err)
		}
	}

	// Prove the lookup WORKS, rather than merely existing.
	//
	// The function body reads pg_authid, which is superuser-only, and it runs
	// as t.AdminUser. An admin with CREATEROLE but not SUPERUSER — which this
	// operator otherwise supports on purpose, see roleOwnerMarker — can create
	// this function and have every call fail with permission denied. That
	// failure would surface at client login, long after Ensure reported Ready
	// with a URI that cannot connect: precisely the shape of bug this whole
	// bootstrap exists to remove, reintroduced one level down.
	//
	// Calling it as the admin exercises the same rights the pooler's call will,
	// because SECURITY DEFINER runs as the owner either way.
	var probeUser string
	if err := dbConn.QueryRow(ctx,
		fmt.Sprintf("SELECT username FROM %s($1)", qFunc), database,
	).Scan(&probeUser); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("pooler auth lookup in %q returned no row for %q, so the pooler cannot authenticate it",
				database, database)
		}
		return fmt.Errorf("pooler auth lookup in %q failed — %s must be able to read pg_authid (SUPERUSER) for pooler access to work; set MIMIR_%s_POOLER_AUTH_ROLE=\"\" to publish direct-to-primary access instead: %w",
			database, t.AdminUser, strings.ToUpper(string(mimirv1alpha1.EnginePostgres)), err)
	}
	return nil
}

// poolerAuthFunctionIsForeign reports whether a get_auth(TEXT) exists that is
// not the one this operator writes — a different owner or a different return
// type. Absent is not foreign: there is simply nothing to replace.
func (Postgres) poolerAuthFunctionIsForeign(ctx context.Context, conn *pgx.Conn, admin string) (bool, error) {
	var owner, result string
	err := conn.QueryRow(ctx, `
		SELECT p.proowner::regrole::text, pg_catalog.pg_get_function_result(p.oid)
		  FROM pg_catalog.pg_proc p
		  JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
		 WHERE n.nspname = $1
		   AND p.proname = $2
		   -- Matched on argument TYPES via proargtypes, not on a rendered
		   -- argument list: pg_get_function_identity_arguments includes the
		   -- parameter NAME ("p_username text"), so comparing it to "text"
		   -- silently matches nothing and the guard never fires.
		   AND p.pronargs = 1
		   AND p.proargtypes[0] = 'text'::pg_catalog.regtype`,
		poolerAuthSchema, poolerAuthFunction,
	).Scan(&owner, &result)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// regrole renders a name needing quotes with them, so compare both forms
	// rather than assuming the admin role name is a bare identifier.
	ownedByAdmin := owner == admin || owner == quoteIdentifier(admin)
	return !ownedByAdmin || result != getAuthResultType, nil
}

// ValidateExtensions reports whether every requested extension is allowed.
//
// Exported so the controller can reject a bad request up front, as a terminal
// InvalidSpec, rather than discovering it at the very end of Ensure — after the
// role, database, marker, password and grants have all been mutated, leaving
// partial state behind and retrying every thirty seconds on a spec that can
// never succeed.
func ValidateExtensions(exts []string) error {
	for _, ext := range exts {
		if !allowedExtensions[ext] {
			return fmt.Errorf("extension %q is not on the allowlist for shared clusters (allowed: %s)",
				ext, strings.Join(AllowedExtensions(), ", "))
		}
	}
	return nil
}

// allowedExtensions is the set a tenant may request.
//
// CREATE EXTENSION runs with administrative rights, and on a SHARED cluster
// that is a privilege boundary rather than a convenience: several contrib
// extensions can read the filesystem or execute arbitrary code as the server
// user, which would reach every other tenant on the instance. These are the
// ones that only affect the tenant's own database.
var allowedExtensions = map[string]bool{
	"btree_gin":          true,
	"btree_gist":         true,
	"citext":             true,
	"hstore":             true,
	"intarray":           true,
	"ltree":              true,
	"pg_stat_statements": true,
	"pg_trgm":            true,
	"pgcrypto":           true,
	"unaccent":           true,
	"uuid-ossp":          true,
	"vector":             true,
}

// AllowedExtensions lists what a tenant may ask for, for error messages and
// documentation. Sorted so the output is stable.
func AllowedExtensions() []string {
	out := make([]string, 0, len(allowedExtensions))
	for k := range allowedExtensions {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ensureExtensions connects to the tenant database itself — CREATE EXTENSION
// is per-database, so it cannot be run from the admin database.
func (p Postgres) ensureExtensions(ctx context.Context, t Target, database string, exts []string) error {
	conn, err := p.connect(ctx, t, database)
	if err != nil {
		return fmt.Errorf("connect to %q for extensions: %w", database, err)
	}
	defer func() { _ = conn.Close(ctx) }()

	// Re-checked here as well as in the controller: this is the boundary that
	// actually reaches an administrative connection, and it must not depend on
	// a caller having validated first.
	if err := ValidateExtensions(exts); err != nil {
		return err
	}

	for _, ext := range exts {
		if _, err := conn.Exec(ctx,
			fmt.Sprintf("CREATE EXTENSION IF NOT EXISTS %s", quoteIdentifier(ext)),
		); err != nil {
			return fmt.Errorf("create extension %q in %q: %w", ext, database, err)
		}
	}
	return nil
}

// Drop removes the database and its role, but ONLY if owner matches the marker
// recorded at creation.
//
// The ownership check here is not symmetry with Ensure — it closes a data-loss
// path that the conflict handling itself opened. The finalizer is added before
// Ensure runs, so a DataService that loses an ownership conflict still carries
// one. Deleting that rejected object used to call Drop by name and destroy the
// *legitimate* tenant's database. A mismatch is therefore "nothing here belongs
// to me", not an error: there is nothing to clean up, and deletion proceeds.
//
// An empty owner skips the check, for callers with no marker to match.
func (p Postgres) Drop(ctx context.Context, t Target, database, owner string) error {
	if err := ValidateIdentifier(database); err != nil {
		return err
	}
	conn, err := p.connect(ctx, t, t.AdminDatabase)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()

	if owner != "" {
		var exists bool
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, database,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check database %q: %w", database, err)
		}
		if exists {
			marker, err := p.databaseOwnerMarker(ctx, conn, database)
			if err != nil {
				return err
			}
			if marker != owner {
				// Someone else's database, or one made by hand. Leave both it
				// and the role alone.
				return nil
			}
		}
	}

	// WITH (FORCE) terminates existing sessions. Without it a single idle
	// client holds the drop open indefinitely and deletion hangs on a
	// finalizer with no obvious cause.
	if _, err := conn.Exec(ctx,
		fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", quoteIdentifier(database)),
	); err != nil {
		return fmt.Errorf("drop database %q: %w", database, err)
	}

	// The role is dropped only when we own it too, checked separately because a
	// role can outlive its database.
	if owner != "" {
		marker, err := p.roleOwnerMarker(ctx, conn, database)
		if err != nil {
			return err
		}
		if marker != owner {
			return nil
		}
	}
	if _, err := conn.Exec(ctx,
		fmt.Sprintf("DROP ROLE IF EXISTS %s", quoteIdentifier(database)),
	); err != nil {
		return fmt.Errorf("drop role %q: %w", database, err)
	}
	return nil
}

// connect opens an ADMIN connection, always against the primary rather than
// the pooler: CREATE DATABASE cannot run inside a transaction block, and a
// pooler in transaction mode wraps every statement in one.
func (Postgres) connect(ctx context.Context, t Target, database string) (*pgx.Conn, error) {
	sslmode := "disable"
	if t.TLS {
		// require, not verify-full: the server cert is issued by the operator's
		// own internal CA, which this client does not carry. The transport is
		// encrypted; the identity check is a hardening follow-up.
		sslmode = "require"
	}
	dsn := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=%s",
		url.QueryEscape(t.AdminUser), url.QueryEscape(t.AdminPassword),
		hostPort(t.AdminHost, t.AdminPort), url.PathEscape(database), sslmode)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		// The DSN carries the admin password, so it must never reach an error
		// message that lands in a log or a status condition.
		return nil, fmt.Errorf("connect to %s:%d/%s as %s: %w", t.AdminHost, t.AdminPort, database, t.AdminUser, err)
	}
	return conn, nil
}

func postgresURI(t Target, database, user, password string) string {
	sslmode := "disable"
	if t.TLS {
		sslmode = "require"
	}
	return fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=%s",
		url.QueryEscape(user), url.QueryEscape(password),
		hostPort(t.Host, t.Port), url.PathEscape(database), sslmode)
}

// hostPort joins a host and port for a URL authority.
//
// net.JoinHostPort rather than "%s:%d" so an IPv6 literal gets its brackets —
// without them the address's own colons are read as the port separator and the
// URL fails to parse. Service DNS names are the normal case, but the published
// URI goes into a Secret that consumers paste into their own config, so it has
// to be correct for whatever host the platform is configured with.
func hostPort(host string, port int32) string {
	return net.JoinHostPort(host, strconv.Itoa(int(port)))
}

// ValidateIdentifier rejects anything that is not a plain lower-case SQL
// identifier. The CRD pattern already enforces this for databaseName, but
// extensions and any future caller arrive unchecked, and these strings are
// interpolated into DDL.
func ValidateIdentifier(s string) error {
	if s == "" {
		return fmt.Errorf("identifier must not be empty")
	}
	if len(s) > 63 {
		return fmt.Errorf("identifier %q exceeds 63 characters", s)
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return fmt.Errorf("identifier %q contains an illegal character %q", s, r)
		}
	}
	return nil
}

// quoteIdentifier double-quotes an identifier, doubling any embedded quote.
// ValidateIdentifier makes that impossible today; this stays correct anyway so
// the two are not coupled.
func quoteIdentifier(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// quoteLiteral single-quotes a string literal, doubling embedded quotes.
// Passwords are generated here and base64url-encoded, so they contain no
// quotes — but a literal built by concatenation is exactly where that
// assumption stops being true later.
func quoteLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}

func generatePassword() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	// base64url avoids the +/= that need escaping in a connection URI.
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
