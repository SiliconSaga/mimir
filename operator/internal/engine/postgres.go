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

// getAuthDDL is a FALLBACK password-lookup function, written only into a
// database that does not already have one. See ensurePoolerAuth for why that
// is usually not the case.
//
// The parameter is named `username`, matching what Percona's PostgreSQL
// operator installs, and that is not cosmetic. CREATE OR REPLACE cannot rename
// an existing function's parameter, so a different name here makes ours and
// theirs mutually unwritable — whichever landed first wins and the other fails
// with SQLSTATE 42P13 on every attempt, forever. An earlier revision used
// `p_username` and wedged provisioning cluster-wide for exactly that reason.
//
// SECURITY DEFINER is unavoidable: pg_authid holds the password verifiers and
// is superuser-only, while the caller is an unprivileged pooler role. That
// makes SET search_path mandatory rather than tidy. The function lands in a
// database the TENANT owns, so without a pinned search_path the tenant could
// create pg_catalog-shadowing objects and have this run with the admin role's
// rights — a clean privilege escalation out of their own database. The path is
// pinned and every reference inside the body is schema-qualified as well.
//
// rolcanlogin filters out group roles, which have no password to hand back.
// The other exclusions mirror Percona's: never hand the pooler a superuser or
// replication verifier, and never its own.
const getAuthDDL = `CREATE OR REPLACE FUNCTION ` + poolerAuthSchema + `.` + poolerAuthFunction + `(username TEXT)
  RETURNS TABLE (username TEXT, password TEXT)
  LANGUAGE sql
  STABLE
  SECURITY DEFINER
  SET search_path = pg_catalog
AS $$
  SELECT rolname::TEXT, rolpassword::TEXT
    FROM pg_catalog.pg_authid
   WHERE rolname = $1
     AND rolcanlogin
     AND NOT rolsuper
     AND NOT rolreplication
     AND (rolvaliduntil IS NULL OR rolvaliduntil >= CURRENT_TIMESTAMP)
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
	qAdmin := quoteIdentifier(t.AdminUser)

	// Whose function is this, if there is one at all?
	//
	// Usually there IS one already, and it is not ours. Percona's PostgreSQL
	// operator installs pgbouncer.get_auth into template1, so every database
	// CREATE DATABASE makes inherits the schema, the function and its grants.
	// Theirs is also stricter than the fallback below — it refuses to return a
	// verifier for a superuser, a replication role, or the pooler itself.
	//
	// So the rule is: do not touch a function the server put there. Replacing
	// it on every reconcile would quietly swap the cluster operator's security
	// decisions for ours, and it sits on the client login path.
	//
	// "The server put it there" is judged by SUPERUSER, not by matching our own
	// admin name. Percona creates the function as `postgres`, while
	// MIMIR_POSTGRES_ADMIN_SECRET may legitimately name a different superuser —
	// and an owner check that only accepted our own name would then drop the
	// cluster operator's function and install our weaker one in its place. Our
	// own admin is accepted too, so a fallback we wrote is not re-created on
	// every pass when that admin is not a superuser.
	owner, isSuper, present, err := p.poolerAuthFunctionOwner(ctx, dbConn)
	if err != nil {
		return fmt.Errorf("inspect pooler auth function in %q: %w", database, err)
	}
	preserve := present && (isSuper || owner == t.AdminUser || owner == qAdmin)

	if !preserve {
		// Either nothing is there — a server with no such pooler integration,
		// where the fallback is what makes the published URI usable at all —
		// or an unprivileged role put it there, which on a database the tenant
		// owns means the tenant. A definer function of theirs would run as
		// them, so it is dropped rather than replaced: CREATE OR REPLACE can
		// change neither an owner nor a return type.
		stmts := []string{
			fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", qSchema),
			fmt.Sprintf("ALTER SCHEMA %s OWNER TO %s", qSchema, qAdmin),
		}
		if present {
			stmts = append(stmts, fmt.Sprintf("DROP FUNCTION IF EXISTS %s(TEXT)", qFunc))
		}
		stmts = append(stmts,
			getAuthDDL,
			fmt.Sprintf("ALTER FUNCTION %s(TEXT) OWNER TO %s", qFunc, qAdmin),
		)
		for _, stmt := range stmts {
			if _, err := dbConn.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("bootstrap pooler auth in %q: %w", database, err)
			}
		}
	}

	// Privileges are reasserted on BOTH paths, and the revokes matter more than
	// the grants.
	//
	// PostgreSQL grants EXECUTE on a new function to PUBLIC by default. This
	// one returns the password verifier of every login role on the shared
	// cluster, so a preserved function that kept that default would let any
	// tenant read every other tenant's credential out of its own database —
	// a worse leak than the isolation this operator exists to provide, and one
	// we would be inheriting rather than causing. Percona happens to revoke it,
	// but "happens to" is not a property to build tenant isolation on.
	//
	// Narrowing someone else's object is not the same as redefining it: the
	// authentication policy in the function body stays theirs.
	for _, stmt := range []string{
		fmt.Sprintf("REVOKE ALL ON SCHEMA %s FROM PUBLIC", qSchema),
		fmt.Sprintf("REVOKE ALL ON FUNCTION %s(TEXT) FROM PUBLIC", qFunc),
		fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s", qSchema, qAuth),
		fmt.Sprintf("GRANT EXECUTE ON FUNCTION %s(TEXT) TO %s", qFunc, qAuth),
	} {
		if _, err := dbConn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("scope pooler auth in %q: %w", database, err)
		}
	}

	return p.verifyPoolerAuth(ctx, dbConn, t, database, authRole, qFunc)
}

// verifyPoolerAuth proves the lookup WORKS, rather than merely existing.
//
// Two ways it can exist and still not work, both of which surface at client
// login rather than here — which is to say, long after Ensure has reported
// Ready with a URI that cannot connect. That is the exact shape of bug this
// bootstrap exists to remove, and it would be a poor joke to reintroduce it
// one level down.
//
//   - The body reads pg_authid, which is superuser-only, and runs as the admin
//     role. An admin with CREATEROLE but not SUPERUSER — which this operator
//     otherwise supports on purpose, see roleOwnerMarker — can create the
//     function and have every call fail with permission denied.
//   - The pooler role may lack USAGE or EXECUTE. Calling the function here
//     does NOT test that: SECURITY DEFINER runs as the owner, and the admin
//     reaches it by ownership regardless of what the pooler was granted.
func (Postgres) verifyPoolerAuth(ctx context.Context, dbConn *pgx.Conn, t Target, database, authRole, qFunc string) error {
	var probeUser string
	if err := dbConn.QueryRow(ctx,
		fmt.Sprintf("SELECT username FROM %s($1)", qFunc), database,
	).Scan(&probeUser); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("pooler auth lookup in %q returned no row for %q, so the pooler cannot authenticate it",
				database, database)
		}
		return fmt.Errorf("pooler auth lookup in %q failed — %s must be able to read pg_authid (SUPERUSER) for pooler access to work, or set MIMIR_%s_POOLER_AUTH_ROLE to the empty string to publish direct-to-primary access instead: %w",
			database, t.AdminUser, strings.ToUpper(string(mimirv1alpha1.EnginePostgres)), err)
	}

	var canUse, canExec bool
	if err := dbConn.QueryRow(ctx, `
		SELECT pg_catalog.has_schema_privilege($1, $2, 'USAGE'),
		       pg_catalog.has_function_privilege($1, $3, 'EXECUTE')`,
		authRole, poolerAuthSchema, poolerAuthSchema+"."+poolerAuthFunction+"(text)",
	).Scan(&canUse, &canExec); err != nil {
		return fmt.Errorf("check pooler auth privileges in %q: %w", database, err)
	}
	if !canUse || !canExec {
		return fmt.Errorf("pooler role %q cannot reach the auth lookup in %q (schema usage=%t, function execute=%t), so the published URI cannot authenticate",
			authRole, database, canUse, canExec)
	}

	// And the other direction: nobody ELSE may reach it. This function hands
	// back the password verifier of every login role on the shared cluster, so
	// PUBLIC being able to call it turns each tenant's own database into a
	// window onto every other tenant's credential. The revokes above are meant
	// to guarantee that; this asserts it, because a revoke that silently did
	// not apply is indistinguishable from one that did.
	var publicUse, publicExec bool
	if err := dbConn.QueryRow(ctx, `
		SELECT pg_catalog.has_schema_privilege('public', $1, 'USAGE'),
		       pg_catalog.has_function_privilege('public', $2, 'EXECUTE')`,
		poolerAuthSchema, poolerAuthSchema+"."+poolerAuthFunction+"(text)",
	).Scan(&publicUse, &publicExec); err != nil {
		return fmt.Errorf("check public access to the auth lookup in %q: %w", database, err)
	}
	if publicUse && publicExec {
		return fmt.Errorf("PUBLIC can execute the auth lookup in %q, which would let any tenant read every login role's password verifier — refusing to publish this database",
			database)
	}
	return nil
}

// poolerAuthFunctionOwner returns the owner of get_auth(TEXT), whether that
// owner is a superuser, and whether the function exists at all.
//
// rolsuper comes from pg_roles rather than pg_authid: pg_authid is readable by
// superusers only, and this runs before anything has established that the
// configured admin is one. pg_roles is the public view over the same rows.
//
// Matched on argument TYPES via proargtypes rather than on a rendered argument
// list: pg_get_function_identity_arguments includes the parameter NAME
// ("username text"), so comparing it to "text" silently matches nothing and
// every caller reads absent.
func (Postgres) poolerAuthFunctionOwner(ctx context.Context, conn *pgx.Conn) (string, bool, bool, error) {
	var owner string
	var isSuper bool
	err := conn.QueryRow(ctx, `
		SELECT r.rolname, r.rolsuper
		  FROM pg_catalog.pg_proc p
		  JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
		  JOIN pg_catalog.pg_roles r ON r.oid = p.proowner
		 WHERE n.nspname = $1
		   AND p.proname = $2
		   AND p.pronargs = 1
		   AND p.proargtypes[0] = 'text'::pg_catalog.regtype`,
		poolerAuthSchema, poolerAuthFunction,
	).Scan(&owner, &isSuper)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, err
	}
	return owner, isSuper, true, nil
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
	// connect_timeout for the same reason the MySQL engine sets its three: the
	// driver waits forever by default and Reconcile carries no deadline, so an
	// unreachable server holds a worker rather than failing and requeueing.
	// Flagged on the MySQL side in review and fixed here too — the exposure was
	// never engine-specific, it was just found there first.
	dsn := fmt.Sprintf("postgres://%s@%s/%s?sslmode=%s&connect_timeout=%d",
		url.UserPassword(t.AdminUser, t.AdminPassword).String(),
		hostPort(t.AdminHost, t.AdminPort), url.PathEscape(database), sslmode,
		int(dialTimeout.Seconds()))

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		// The DSN carries the admin password, so it must never reach an error
		// message that lands in a log or a status condition.
		return nil, fmt.Errorf("connect to %s:%d/%s as %s: %w", t.AdminHost, t.AdminPort, database, t.AdminUser, err)
	}
	return conn, nil
}

// postgresURI builds the connection string published to the consuming app.
//
// url.UserPassword rather than QueryEscape on each half: QueryEscape encodes a
// space as `+`, which is only a space in a query string. In URI userinfo `+` is
// a literal plus, so a credential containing a space would be handed to the
// consumer verbatim-wrong and authentication would fail with nothing to point
// at. url.UserPassword uses the userinfo escaping rules, where a space becomes
// %20 and `:` `@` `/` `?` are escaped as well.
//
// Latent rather than live for tenants — generated passwords are base64url, so
// none of them can contain a space — but this string goes into a Secret that
// consumers paste into their own config, and admin credentials on the connect
// path are operator-supplied and under no such constraint.
func postgresURI(t Target, database, user, password string) string {
	sslmode := "disable"
	if t.TLS {
		sslmode = "require"
	}
	return fmt.Sprintf("postgres://%s@%s/%s?sslmode=%s",
		url.UserPassword(user, password).String(),
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
