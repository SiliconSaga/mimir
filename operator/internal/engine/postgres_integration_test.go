//go:build integration

// Integration tests for the PostgreSQL provisioner against a real server.
//
// The unit tests cover identifier validation and quoting, which is where a
// SQL-injection bug would live. They cannot cover the thing that actually
// decides whether a shared cluster is safe: whether PostgreSQL really refuses
// a cross-tenant connection after REVOKE CONNECT. That is a property of the
// server, not of this code, so it needs a server.
//
// Run against a throwaway container:
//
//	docker run -d --name mimir-pgtest -e POSTGRES_PASSWORD=testadminpw \
//	  -p 15432:5432 postgres:15
//	MIMIR_TEST_PG=localhost:15432 go test -tags integration ./internal/engine/...
//
// Skipped unless MIMIR_TEST_PG is set, so the default `go test ./...` stays
// hermetic and CI does not need a database.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// testDB returns a database name unique to this run.
//
// Fixed names would collide between two concurrent runs against the same
// server — a stray leftover from a killed run then fails the next one for a
// reason that has nothing to do with the code under test.
func testDB(t *testing.T, stem string) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return stem + "_" + hex.EncodeToString(b[:])
}

func testTarget(t *testing.T) Target {
	t.Helper()
	addr := os.Getenv("MIMIR_TEST_PG")
	if addr == "" {
		t.Skip("MIMIR_TEST_PG not set")
	}
	// SplitHostPort rather than Cut, so a bracketed IPv6 literal parses.
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("MIMIR_TEST_PG must be host:port, got %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("bad port in MIMIR_TEST_PG: %v", err)
	}
	if port < 1 || port > 65535 {
		t.Fatalf("port %d in MIMIR_TEST_PG out of range", port)
	}
	return Target{
		Host: host, Port: int32(port),
		AdminHost: host, AdminPort: int32(port),
		AdminUser: "postgres", AdminPassword: "testadminpw",
		AdminDatabase: "postgres",
		// The throwaway container serves plaintext. Production targets are
		// hostssl-only, which the provisioner handles via Target.TLS.
		TLS: false,
	}
}

// cleanup drops a database on a bounded context and reports failure.
//
// t.Cleanup with context.Background() and a discarded error can hang against an
// unreachable server, and a silent failure leaves databases and roles behind
// while the test still reports success — which then breaks the NEXT run for a
// reason that has nothing to do with the code.
func cleanup(t *testing.T, tgt Target, database string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := (Postgres{}).Drop(ctx, tgt, database, ""); err != nil {
			t.Errorf("cleanup: dropping %q left state behind: %v", database, err)
		}
	})
}

// adminURI is the superuser connection string for direct catalog checks.
func adminURI(tgt Target) string {
	return fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable",
		tgt.AdminUser, tgt.AdminPassword,
		net.JoinHostPort(tgt.AdminHost, strconv.Itoa(int(tgt.AdminPort))), tgt.AdminDatabase)
}

// connectAs opens a connection with an arbitrary credential, which is how a
// tenant's own connection is simulated.
func connectAs(ctx context.Context, tgt Target, user, password, database string) error {
	uri := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable",
		user, password, net.JoinHostPort(tgt.AdminHost, strconv.Itoa(int(tgt.AdminPort))), database)
	conn, err := pgx.Connect(ctx, uri)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	var one int
	return conn.QueryRow(ctx, "SELECT 1").Scan(&one)
}

// TestTenantIsolation is the load-bearing one.
//
// PostgreSQL lets any role connect to any database by default, so without
// REVOKE CONNECT a shared cluster is shared data. This provisions two tenants
// and asserts each is refused by the other's database — and insists on the
// RIGHT failure, because a typo in the hostname also produces an error and
// would otherwise read as a pass.
func TestTenantIsolation(t *testing.T) {
	tgt := testTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	p := Postgres{}
	alphaDB := testDB(t, "tenant_alpha")
	betaDB := testDB(t, "tenant_beta")

	alpha, err := p.Ensure(ctx, tgt, alphaDB, "", Options{})
	if err != nil {
		t.Fatalf("provisioning tenant_alpha: %v", err)
	}
	cleanup(t, tgt, alphaDB)

	beta, err := p.Ensure(ctx, tgt, betaDB, "", Options{})
	if err != nil {
		t.Fatalf("provisioning tenant_beta: %v", err)
	}
	cleanup(t, tgt, betaDB)

	// Each tenant reaches its own database.
	if err := connectAs(ctx, tgt, alpha.Username, alpha.Password, alphaDB); err != nil {
		t.Fatalf("alpha cannot reach its own database: %v", err)
	}
	if err := connectAs(ctx, tgt, beta.Username, beta.Password, betaDB); err != nil {
		t.Fatalf("beta cannot reach its own database: %v", err)
	}

	// Neither reaches the other's, and for the right reason.
	for _, tc := range []struct {
		name, user, password, database string
	}{
		{"alpha into beta", alpha.Username, alpha.Password, betaDB},
		{"beta into alpha", beta.Username, beta.Password, alphaDB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := connectAs(ctx, tgt, tc.user, tc.password, tc.database)
			if err == nil {
				t.Fatal("ISOLATION FAILURE: cross-tenant connection succeeded")
			}
			if !strings.Contains(strings.ToLower(err.Error()), "permission denied") {
				t.Fatalf("refused, but not by the grant model — got %v", err)
			}
		})
	}
}

// TestRefusesToAdoptAnotherOwnersDatabase is the guard on the collision that
// only exists once there is more than one customer.
//
// Two DataServices resolving to the same physical name — different namespaces,
// same object name, or two explicit databaseName values that happen to match —
// must not silently share a database. Before the ownership marker the second
// one would adopt the first's data AND reset its password, so the first tenant
// lost access to a database the second could now read.
func TestRefusesToAdoptAnotherOwnersDatabase(t *testing.T) {
	tgt := testTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	p := Postgres{}
	db := testDB(t, "tenant_contested")

	first, err := p.Ensure(ctx, tgt, db, "", Options{Owner: "team-a/app"})
	if err != nil {
		t.Fatalf("provisioning for team-a: %v", err)
	}
	cleanup(t, tgt, db)

	_, err = p.Ensure(ctx, tgt, db, "", Options{Owner: "team-b/app"})
	if err == nil {
		t.Fatal("OWNERSHIP FAILURE: a second DataService adopted an existing database")
	}
	var notOwned *ErrNotOwned
	if !errors.As(err, &notOwned) {
		t.Fatalf("refused, but not as an ownership conflict — got %v", err)
	}

	// The first tenant's credential must still work. A refusal that already
	// rotated the password would be a failure wearing a success's clothes.
	if err := connectAs(ctx, tgt, first.Username, first.Password, db); err != nil {
		t.Fatalf("first owner locked out by the rejected second claim: %v", err)
	}
}

// TestDropRefusesAnotherOwnersDatabase covers the data-loss path that the
// conflict handling itself opened.
//
// The finalizer is added BEFORE Ensure runs, so a DataService that loses an
// ownership conflict still carries one. Deleting that rejected object called
// Drop by name — and destroyed the winner's database. A mismatched marker must
// mean "nothing here is mine", leaving both database and role untouched.
func TestDropRefusesAnotherOwnersDatabase(t *testing.T) {
	tgt := testTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	p := Postgres{}
	db := testDB(t, "tenant_victim")

	owner, err := p.Ensure(ctx, tgt, db, "", Options{Owner: "team-a/app"})
	if err != nil {
		t.Fatalf("provisioning for team-a: %v", err)
	}
	cleanup(t, tgt, db)

	// The loser of the conflict tries to clean up on deletion.
	if err := p.Drop(ctx, tgt, db, "team-b/app"); err != nil {
		t.Fatalf("Drop by a non-owner should be a no-op, got %v", err)
	}

	// The database must still exist and still work for its real owner.
	if err := connectAs(ctx, tgt, owner.Username, owner.Password, db); err != nil {
		t.Fatalf("DATA LOSS: a non-owner's delete destroyed the owner's database: %v", err)
	}

	// And the owner can still drop it.
	if err := p.Drop(ctx, tgt, db, "team-a/app"); err != nil {
		t.Fatalf("owner cannot drop its own database: %v", err)
	}

	admin, err := pgx.Connect(ctx, adminURI(tgt))
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer admin.Close(ctx)
	var n int
	if err := admin.QueryRow(ctx,
		"SELECT count(*) FROM pg_database WHERE datname = $1", db).Scan(&n); err != nil {
		t.Fatalf("querying pg_database: %v", err)
	}
	if n != 0 {
		t.Error("owner's own Drop did not remove the database")
	}
}

// TestRecoversFromInterruptedCreate covers the crash window that the ownership
// marker itself opened.
//
// CREATE DATABASE cannot run inside a transaction, so a crash between creating
// the database and stamping its marker leaves an unmarked database. Rejecting
// that outright wedges the request forever — and because status is never
// populated, deletion has no name to clean up either. Adoption is allowed only
// on proof: the database is owned by a role already carrying our marker, which
// only we could have arranged.
func TestRecoversFromInterruptedCreate(t *testing.T) {
	tgt := testTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	p := Postgres{}
	db := testDB(t, "tenant_interrupted")
	const owner = "team-a/app/uid-1111"

	// Provision normally, then strip the database marker to reproduce the
	// state a crash between CREATE and COMMENT would leave behind.
	if _, err := p.Ensure(ctx, tgt, db, "", Options{Owner: owner}); err != nil {
		t.Fatalf("initial provisioning: %v", err)
	}
	cleanup(t, tgt, db)

	admin, err := pgx.Connect(ctx, adminURI(tgt))
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "COMMENT ON DATABASE "+quoteIdentifier(db)+" IS NULL"); err != nil {
		t.Fatalf("clearing the database marker: %v", err)
	}

	// The next reconcile must recover rather than wedge.
	creds, err := p.Ensure(ctx, tgt, db, "", Options{Owner: owner})
	if err != nil {
		t.Fatalf("did not recover from an interrupted create: %v", err)
	}
	if err := connectAs(ctx, tgt, creds.Username, creds.Password, db); err != nil {
		t.Fatalf("recovered database is not usable: %v", err)
	}

	// And the marker is back, so a different owner is refused again.
	if _, err := p.Ensure(ctx, tgt, db, "", Options{Owner: "team-b/app/uid-2222"}); err == nil {
		t.Fatal("recovery left the database adoptable by anyone")
	}
}

// TestReassertsDatabaseOwner covers an administrator reassigning ownership by
// hand. The API promises an owning role, so losing it must not read as Ready.
func TestReassertsDatabaseOwner(t *testing.T) {
	tgt := testTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	p := Postgres{}
	db := testDB(t, "tenant_ownerdrift")
	const owner = "team-a/app/uid-3333"

	if _, err := p.Ensure(ctx, tgt, db, "", Options{Owner: owner}); err != nil {
		t.Fatalf("provisioning: %v", err)
	}
	cleanup(t, tgt, db)

	admin, err := pgx.Connect(ctx, adminURI(tgt))
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx,
		"ALTER DATABASE "+quoteIdentifier(db)+" OWNER TO postgres"); err != nil {
		t.Fatalf("reassigning ownership: %v", err)
	}

	if _, err := p.Ensure(ctx, tgt, db, "", Options{Owner: owner}); err != nil {
		t.Fatalf("reconcile after ownership drift: %v", err)
	}

	var got string
	if err := admin.QueryRow(ctx,
		`SELECT r.rolname FROM pg_database d JOIN pg_roles r ON r.oid = d.datdba
		  WHERE d.datname = $1`, db).Scan(&got); err != nil {
		t.Fatalf("reading owner: %v", err)
	}
	if got != db {
		t.Errorf("owner not restored: database is owned by %q, want %q", got, db)
	}
}

// TestOwnerMarkerIsPerObjectNotPerName covers the force-delete orphan.
//
// Deleting a DataService whose database could not be dropped leaves the
// database behind. Recreating an object with the SAME namespace and name must
// not inherit it — with a name-only marker it would, silently adopting the
// orphan and rotating its password.
func TestOwnerMarkerIsPerObjectNotPerName(t *testing.T) {
	tgt := testTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	p := Postgres{}
	db := testDB(t, "tenant_reincarnated")

	// Same namespace and name, different object identity.
	const first = "team-a/app/uid-aaaa"
	const second = "team-a/app/uid-bbbb"

	if _, err := p.Ensure(ctx, tgt, db, "", Options{Owner: first}); err != nil {
		t.Fatalf("provisioning the original: %v", err)
	}
	cleanup(t, tgt, db)

	_, err := p.Ensure(ctx, tgt, db, "", Options{Owner: second})
	var notOwned *ErrNotOwned
	if !errors.As(err, &notOwned) {
		t.Fatalf("a replacement object inherited the orphaned database, got %v", err)
	}
}

// TestRefusesToAdoptAnUnmarkedRole guards the privilege-escalation shape of the
// same problem. Adopting an existing role by name hands the tenant whatever
// that role already carries, and then publishes a password for it.
func TestRefusesToAdoptAnUnmarkedRole(t *testing.T) {
	tgt := testTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	name := testDB(t, "tenant_roleclash")

	admin, err := pgx.Connect(ctx, adminURI(tgt))
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer admin.Close(ctx)

	// A role that predates the operator, as a hand-made one would.
	if _, err := admin.Exec(ctx, "CREATE ROLE "+quoteIdentifier(name)+" WITH LOGIN"); err != nil {
		t.Fatalf("seeding a hand-made role: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		c, err := pgx.Connect(cctx, adminURI(tgt))
		if err != nil {
			t.Errorf("cleanup: cannot reach the server to drop role %q: %v", name, err)
			return
		}
		defer c.Close(cctx)
		if _, err := c.Exec(cctx, "DROP ROLE IF EXISTS "+quoteIdentifier(name)); err != nil {
			t.Errorf("cleanup: dropping role %q left state behind: %v", name, err)
		}
	})

	_, err = Postgres{}.Ensure(ctx, tgt, name, "", Options{Owner: "team-a/app"})
	var notOwned *ErrNotOwned
	if !errors.As(err, &notOwned) {
		t.Fatalf("expected an ownership conflict for an unmarked role, got %v", err)
	}
}

// A database created by hand, with no marker, must also be left alone —
// adopting it would hand a tenant data the operator did not create.
func TestRefusesUnmarkedPreExistingDatabase(t *testing.T) {
	tgt := testTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	preDB := testDB(t, "tenant_preexisting")

	admin := adminURI(tgt)
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, "CREATE DATABASE "+quoteIdentifier(preDB)); err != nil {
		t.Fatalf("seeding a hand-made database: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		c, err := pgx.Connect(cctx, admin)
		if err != nil {
			t.Errorf("cleanup: cannot reach the server to drop %q: %v", preDB, err)
			return
		}
		defer c.Close(cctx)
		if _, err := c.Exec(cctx, "DROP DATABASE IF EXISTS "+quoteIdentifier(preDB)); err != nil {
			t.Errorf("cleanup: dropping %q left state behind: %v", preDB, err)
		}
	})

	_, err = Postgres{}.Ensure(ctx, tgt, preDB, "", Options{Owner: "team-a/app"})
	var notOwned *ErrNotOwned
	if !errors.As(err, &notOwned) {
		t.Fatalf("expected an ownership conflict for an unmarked database, got %v", err)
	}
}

// TestEnsureIsIdempotent covers the common case: Ensure runs on every
// reconcile, roughly every ten minutes, and almost always has nothing to do.
//
// It also pins the password-stability contract. Generating a fresh password
// each pass would rotate the credential out from under every consumer that
// cached it, so a second call with the current value must return it unchanged.
func TestEnsureIsIdempotent(t *testing.T) {
	tgt := testTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	p := Postgres{}
	idemDB := testDB(t, "tenant_idem")

	first, err := p.Ensure(ctx, tgt, idemDB, "", Options{})
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	cleanup(t, tgt, idemDB)

	second, err := p.Ensure(ctx, tgt, idemDB, first.Password, Options{})
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if second.Password != first.Password {
		t.Error("password changed on reconcile — every consumer would break")
	}
	if second.URI != first.URI {
		t.Errorf("URI changed on reconcile:\n  %s\n  %s", first.URI, second.URI)
	}

	// The credential still works after the second pass, which is what proves
	// the re-applied ALTER ROLE agrees with the Secret rather than drifting.
	if err := connectAs(ctx, tgt, second.Username, second.Password, idemDB); err != nil {
		t.Fatalf("credential broken after reconcile: %v", err)
	}
}

// TestDropRemovesEverything checks deletion leaves nothing behind — a
// leftover role would collide the next time the same database name is asked
// for, which in a homelab is often the very next thing that happens.
func TestDropRemovesEverything(t *testing.T) {
	tgt := testTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	p := Postgres{}
	dropDB := testDB(t, "tenant_drop")

	creds, err := p.Ensure(ctx, tgt, dropDB, "", Options{})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := p.Drop(ctx, tgt, dropDB, ""); err != nil {
		t.Fatalf("Drop: %v", err)
	}

	admin := adminURI(tgt)
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer conn.Close(ctx)

	var n int
	if err := conn.QueryRow(ctx,
		"SELECT count(*) FROM pg_database WHERE datname = $1", dropDB).Scan(&n); err != nil {
		t.Fatalf("querying pg_database: %v", err)
	}
	if n != 0 {
		t.Error("database survived Drop")
	}

	if err := conn.QueryRow(ctx,
		"SELECT count(*) FROM pg_roles WHERE rolname = $1", creds.Username).Scan(&n); err != nil {
		t.Fatalf("querying pg_roles: %v", err)
	}
	if n != 0 {
		t.Errorf("role %q survived Drop — the next request for this name would collide", creds.Username)
	}

	// Re-provisioning the same name must work, which is the practical
	// consequence of the two checks above.
	if _, err := p.Ensure(ctx, tgt, dropDB, "", Options{}); err != nil {
		t.Fatalf("re-provisioning after Drop: %v", err)
	}
	cleanup(t, tgt, dropDB)
}

// tenantURI is a connection string for an arbitrary credential against a named
// database, on the ADMIN endpoint — the tests talk to a bare server with no
// pooler in front, so that is the only endpoint there is.
func tenantURI(tgt Target, user, password, database string) string {
	return fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable",
		user, password,
		net.JoinHostPort(tgt.AdminHost, strconv.Itoa(int(tgt.AdminPort))), database)
}

// poolerAuthPassword is fixed because the role is created and thrown away
// within a single test; nothing outside it ever sees the value.
const poolerAuthPassword = "poolertestpw"

// createPoolerRole makes a stand-in for _crunchypgbouncer and drops it again.
//
// Named per run so two concurrent runs against one server cannot drop each
// other's role mid-test. The cleanup opens its OWN connection rather than
// closing over the caller's: t.Cleanup runs after the test function returns,
// which is after its defers, so a captured connection is already closed by
// then — and the role would silently survive to collide with a later run.
func createPoolerRole(t *testing.T, tgt Target) string {
	t.Helper()
	role := testDB(t, "pooler_auth")

	exec := func(sql string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, adminURI(tgt))
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close(ctx) }()
		_, err = conn.Exec(ctx, sql)
		return err
	}

	if err := exec(fmt.Sprintf("CREATE ROLE %s WITH LOGIN PASSWORD %s",
		quoteIdentifier(role), quoteLiteral(poolerAuthPassword))); err != nil {
		t.Fatalf("create pooler role: %v", err)
	}
	t.Cleanup(func() {
		if err := exec("DROP ROLE IF EXISTS " + quoteIdentifier(role)); err != nil {
			t.Errorf("cleanup: dropping pooler role %q: %v", role, err)
		}
	})
	return role
}

// TestPoolerAuthBootstrap covers the reason the published URI works at all.
//
// A pooler does not verify passwords itself: it connects as its own role into
// the database the client named and runs an auth_query there. `REVOKE CONNECT
// ... FROM PUBLIC` — the revoke that makes the shared cluster multi-tenant —
// catches that role too, and the lookup function does not exist in a database
// this operator created. So before ensurePoolerAuth every connection through
// the pooler was refused, for every tenant, including to its own database.
//
// That failure was invisible from outside: pgBouncer reports the failed lookup
// to the client as `permission denied for database "x"`, character for
// character what a correctly refused CROSS-tenant attempt produces. The
// end-to-end isolation assertion therefore passed while nothing worked at all.
//
// This test does what pgBouncer does, directly, so it can fail for the right
// reason rather than an indistinguishable one.
func TestPoolerAuthBootstrap(t *testing.T) {
	tgt := testTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	authRole := createPoolerRole(t, tgt)
	tgt.PoolerAuthRole = authRole

	p := Postgres{}
	db := testDB(t, "pooled")
	// Registered BEFORE Ensure, unlike the tests above. Ensure now probes the
	// lookup and can fail after the database exists and has granted the pooler
	// role CONNECT — and that grant makes the role undroppable, so a cleanup
	// registered only on success buries the real failure under a confusing
	// "cannot be dropped because some objects depend on it".
	cleanup(t, tgt, db)
	creds, err := p.Ensure(ctx, tgt, db, "", Options{Owner: "ns/pooled/uid-1"})
	if err != nil {
		t.Fatalf("provisioning: %v", err)
	}

	// What pgBouncer actually does: connect AS the auth role INTO the tenant's
	// database, then look the tenant's password up there.
	poolerConn, err := pgx.Connect(ctx, tenantURI(tgt, authRole, poolerAuthPassword, db))
	if err != nil {
		t.Fatalf("the pooler role cannot reach %q, so every client through the pooler is refused: %v", db, err)
	}
	defer func() { _ = poolerConn.Close(ctx) }()

	var gotUser, gotSecret string
	if err := poolerConn.QueryRow(ctx,
		"SELECT username, password FROM pgbouncer.get_auth($1)", creds.Username,
	).Scan(&gotUser, &gotSecret); err != nil {
		t.Fatalf("auth_query failed for %q: %v", creds.Username, err)
	}
	if gotUser != creds.Username {
		t.Fatalf("auth_query returned user %q, want %q", gotUser, creds.Username)
	}
	if gotSecret == "" {
		t.Fatal("auth_query returned an empty verifier, so the pooler cannot authenticate anyone")
	}

	// The tenant must NOT be able to run the lookup themselves. It returns the
	// password verifier of every login role on the cluster, so a copy of it
	// reachable inside each tenant's own database would hand every tenant the
	// credentials of every other one — the opposite of what this operator is
	// for, delivered by the fix for a connectivity bug.
	t.Run("tenant cannot read the lookup", func(t *testing.T) {
		conn, err := pgx.Connect(ctx, tenantURI(tgt, creds.Username, creds.Password, db))
		if err != nil {
			t.Fatalf("tenant cannot reach its own database: %v", err)
		}
		defer func() { _ = conn.Close(ctx) }()

		var u, s string
		err = conn.QueryRow(ctx,
			"SELECT username, password FROM pgbouncer.get_auth($1)", creds.Username).Scan(&u, &s)
		if err == nil {
			t.Fatal("PRIVILEGE LEAK: a tenant can read password verifiers through pgbouncer.get_auth")
		}
		// Name the object that refused. A bare "permission denied" would also
		// be produced by a database the tenant cannot even reach, which is the
		// state before the bootstrap exists — so the generic check would pass
		// while proving nothing about the grants on this function.
		msg := strings.ToLower(err.Error())
		if !strings.Contains(msg, "permission denied for schema") &&
			!strings.Contains(msg, "permission denied for function") {
			t.Fatalf("tenant was refused, but not by the grants on the lookup — got %v", err)
		}
	})

	// SECURITY DEFINER inside a database the TENANT owns is only safe with the
	// search_path pinned and the objects owned by the admin role. Without
	// either, a tenant can shadow pg_catalog or replace the function outright
	// and have it run with the admin role's rights.
	t.Run("definer function is hardened", func(t *testing.T) {
		conn, err := pgx.Connect(ctx, tenantURI(tgt, tgt.AdminUser, tgt.AdminPassword, db))
		if err != nil {
			t.Fatalf("admin connect to %q: %v", db, err)
		}
		defer func() { _ = conn.Close(ctx) }()

		var schemaOwner, funcOwner string
		var proconfig []string
		if err := conn.QueryRow(ctx, `
			SELECT sn.rolname, fn.rolname, p.proconfig
			  FROM pg_proc p
			  JOIN pg_namespace n  ON n.oid = p.pronamespace
			  JOIN pg_roles     sn ON sn.oid = n.nspowner
			  JOIN pg_roles     fn ON fn.oid = p.proowner
			 WHERE n.nspname = 'pgbouncer' AND p.proname = 'get_auth'`,
		).Scan(&schemaOwner, &funcOwner, &proconfig); err != nil {
			t.Fatalf("reading pgbouncer.get_auth from the catalog: %v", err)
		}
		if schemaOwner != tgt.AdminUser {
			t.Errorf("pgbouncer schema owned by %q, want %q — a tenant-owned schema can house a hijacked definer function",
				schemaOwner, tgt.AdminUser)
		}
		if funcOwner != tgt.AdminUser {
			t.Errorf("get_auth owned by %q, want %q", funcOwner, tgt.AdminUser)
		}
		if !strings.Contains(strings.Join(proconfig, ","), "search_path=") {
			t.Errorf("get_auth has no pinned search_path (proconfig=%v) — SECURITY DEFINER in a tenant-owned database without one is a privilege escalation",
				proconfig)
		}
	})

	// Running twice must not trip over the objects created the first time.
	if _, err := p.Ensure(ctx, tgt, db, creds.Password, Options{Owner: "ns/pooled/uid-1"}); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
}

// TestPoolerAuthReclaimsATenantOwnedSchema is the case that makes the
// ownership reassertion worth its lines.
//
// The tenant owns the database, so it can create a schema called `pgbouncer`
// before the operator ever gets there. `CREATE SCHEMA IF NOT EXISTS` would
// then quietly leave it theirs, and `CREATE OR REPLACE FUNCTION` preserves an
// existing owner — so a definer function meant to run as the admin role would
// end up running as the tenant, and the pooler would stop being able to
// authenticate anyone against that database.
func TestPoolerAuthReclaimsATenantOwnedSchema(t *testing.T) {
	tgt := testTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	authRole := createPoolerRole(t, tgt)

	p := Postgres{}
	db := testDB(t, "squatted")

	// First pass with no pooler configured, so the tenant gets a database with
	// no pgbouncer schema in it yet.
	tgt.PoolerAuthRole = ""
	cleanup(t, tgt, db)
	creds, err := p.Ensure(ctx, tgt, db, "", Options{Owner: "ns/squatted/uid-1"})
	if err != nil {
		t.Fatalf("provisioning: %v", err)
	}

	// The tenant squats the name, as it is entitled to do in its own database.
	tenantConn, err := pgx.Connect(ctx, tenantURI(tgt, creds.Username, creds.Password, db))
	if err != nil {
		t.Fatalf("tenant connect: %v", err)
	}
	if _, err := tenantConn.Exec(ctx, "CREATE SCHEMA pgbouncer"); err != nil {
		t.Fatalf("tenant creating the pgbouncer schema: %v", err)
	}
	// A DIFFERENT return type on purpose, because that is the case the
	// bootstrap cannot talk its way out of: CREATE OR REPLACE can change a
	// function's body but not its signature, so this one has to be dropped or
	// every reconcile from here on fails on the DDL and the DataService never
	// reaches Ready.
	if _, err := tenantConn.Exec(ctx, `
		CREATE FUNCTION pgbouncer.get_auth(p_username TEXT)
		  RETURNS TEXT
		  LANGUAGE sql AS $$ SELECT 'nothing'::TEXT $$`,
	); err != nil {
		t.Fatalf("tenant creating a decoy get_auth: %v", err)
	}
	_ = tenantConn.Close(ctx)

	// Now the pooler is configured and the operator reconciles again.
	tgt.PoolerAuthRole = authRole
	if _, err := p.Ensure(ctx, tgt, db, creds.Password, Options{Owner: "ns/squatted/uid-1"}); err != nil {
		t.Fatalf("reconciling over a tenant-owned pgbouncer schema: %v", err)
	}

	// The pooler must now be able to authenticate against it, with the real
	// verifier rather than the tenant's decoy.
	poolerConn, err := pgx.Connect(ctx, tenantURI(tgt, authRole, poolerAuthPassword, db))
	if err != nil {
		t.Fatalf("pooler role cannot reach %q: %v", db, err)
	}
	defer func() { _ = poolerConn.Close(ctx) }()

	var gotUser, gotSecret string
	if err := poolerConn.QueryRow(ctx,
		"SELECT username, password FROM pgbouncer.get_auth($1)", creds.Username,
	).Scan(&gotUser, &gotSecret); err != nil {
		t.Fatalf("auth_query failed after reclaiming the schema: %v", err)
	}
	if gotUser != creds.Username || gotSecret == "nothing" {
		t.Fatalf("auth_query still answering from the tenant's decoy: user=%q secret=%q", gotUser, gotSecret)
	}

	// The replacement must belong to the admin role. A function left owned by
	// the tenant runs as the tenant under SECURITY DEFINER, which fails closed
	// on pg_authid — safe, but it breaks auth for a live service.
	adminConn, err := pgx.Connect(ctx, tenantURI(tgt, tgt.AdminUser, tgt.AdminPassword, db))
	if err != nil {
		t.Fatalf("admin connect to %q: %v", db, err)
	}
	defer func() { _ = adminConn.Close(ctx) }()

	var funcOwner string
	if err := adminConn.QueryRow(ctx, `
		SELECT p.proowner::regrole::text
		  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		 WHERE n.nspname = 'pgbouncer' AND p.proname = 'get_auth'`,
	).Scan(&funcOwner); err != nil {
		t.Fatalf("reading the reclaimed function's owner: %v", err)
	}
	if funcOwner != tgt.AdminUser {
		t.Errorf("reclaimed get_auth owned by %q, want %q", funcOwner, tgt.AdminUser)
	}

	// Reconciling again must be a no-op rather than another drop-and-create.
	// This function is on the client login path, so recreating it every pass
	// would put a window in front of every login.
	if err := (Postgres{}).ensurePoolerAuth(ctx, adminConn, tgt, db); err != nil {
		t.Fatalf("second pooler bootstrap over our own function: %v", err)
	}
	gotOwner, _, present, err := (Postgres{}).poolerAuthFunctionOwner(ctx, adminConn)
	if err != nil {
		t.Fatalf("inspecting the function after reconcile: %v", err)
	}
	if !present || gotOwner != tgt.AdminUser {
		t.Errorf("after reconcile get_auth is present=%t owner=%q, want present owned by %q",
			present, gotOwner, tgt.AdminUser)
	}
}

// TestPoolerAuthLeavesTheServersOwnFunctionAlone is the case that took down
// provisioning on a real cluster.
//
// Percona's PostgreSQL operator installs pgbouncer.get_auth into template1, so
// every database CREATE DATABASE makes already has one — admin-owned, and with
// its input parameter named `username`. An earlier revision wrote its own with
// the parameter named `p_username`, and since CREATE OR REPLACE cannot rename a
// parameter, every DataService on the cluster failed with
// "cannot change name of input parameter" and never reached Ready.
//
// The rule now is that an admin-owned function is the server's business: it is
// left exactly as found. Theirs is stricter than ours anyway — it refuses to
// return a verifier for a superuser or a replication role.
func TestPoolerAuthLeavesTheServersOwnFunctionAlone(t *testing.T) {
	tgt := testTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	authRole := createPoolerRole(t, tgt)

	p := Postgres{}
	db := testDB(t, "preinstalled")

	// Named and scheduled for removal BEFORE the database cleanup is
	// registered, because t.Cleanup runs LIFO: this role ends up owning a
	// function inside that database, so dropping it first fails with "cannot
	// be dropped because some objects depend on it" and buries whatever the
	// test was actually reporting. DROP ROLE IF EXISTS is a no-op if the test
	// never got as far as creating it.
	serverOwner := testDB(t, "srv_owner")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, adminURI(tgt))
		if err != nil {
			t.Errorf("cleanup: connecting to drop %q: %v", serverOwner, err)
			return
		}
		defer func() { _ = conn.Close(ctx) }()
		if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+quoteIdentifier(serverOwner)); err != nil {
			t.Errorf("cleanup: dropping %q: %v", serverOwner, err)
		}
	})
	cleanup(t, tgt, db)

	// Provision with no pooler configured, then plant the server's flavour of
	// the function the way template1 would have.
	tgt.PoolerAuthRole = ""
	creds, err := p.Ensure(ctx, tgt, db, "", Options{})
	if err != nil {
		t.Fatalf("provisioning: %v", err)
	}

	adminConn, err := pgx.Connect(ctx, tenantURI(tgt, tgt.AdminUser, tgt.AdminPassword, db))
	if err != nil {
		t.Fatalf("admin connect to %q: %v", db, err)
	}
	defer func() { _ = adminConn.Close(ctx) }()

	// Owned by a DIFFERENT superuser than the configured admin. Percona creates
	// the function as `postgres`, while MIMIR_POSTGRES_ADMIN_SECRET may name
	// another superuser — and a preserve rule that only recognised our own
	// admin name would drop the cluster operator's function and install our
	// weaker one over it.
	if _, err := adminConn.Exec(ctx,
		fmt.Sprintf("CREATE ROLE %s WITH SUPERUSER", quoteIdentifier(serverOwner)),
	); err != nil {
		t.Fatalf("creating the stand-in server owner: %v", err)
	}

	// Percona's definition, verbatim in the parts that matter: parameter named
	// `username`, and a marker in the body so a silent overwrite is visible.
	//
	// Deliberately left in the DANGEROUS shape a forgetful install produces —
	// PUBLIC has USAGE on the schema and keeps PostgreSQL's default EXECUTE on
	// the function. Preserving the definition must not mean preserving that.
	if _, err := adminConn.Exec(ctx, fmt.Sprintf(`
		CREATE SCHEMA IF NOT EXISTS pgbouncer;
		CREATE OR REPLACE FUNCTION pgbouncer.get_auth(username TEXT)
		  RETURNS TABLE (username TEXT, password TEXT)
		  LANGUAGE sql STABLE SECURITY DEFINER
		AS $$ SELECT rolname::TEXT, rolpassword::TEXT || ''::TEXT
		        FROM pg_catalog.pg_authid
		       WHERE rolname = $1 AND rolcanlogin AND NOT rolsuper $$;
		COMMENT ON FUNCTION pgbouncer.get_auth(TEXT) IS 'installed-by-the-server';
		ALTER FUNCTION pgbouncer.get_auth(TEXT) OWNER TO %s;
		GRANT USAGE ON SCHEMA pgbouncer TO PUBLIC;
		GRANT EXECUTE ON FUNCTION pgbouncer.get_auth(TEXT) TO PUBLIC`,
		quoteIdentifier(serverOwner)),
	); err != nil {
		t.Fatalf("planting the server's get_auth: %v", err)
	}

	// Now reconcile WITH the pooler configured. This is the exact sequence that
	// failed on the cluster.
	tgt.PoolerAuthRole = authRole
	if _, err := p.Ensure(ctx, tgt, db, creds.Password, Options{}); err != nil {
		t.Fatalf("reconciling over the server's own get_auth: %v", err)
	}

	var comment *string
	if err := adminConn.QueryRow(ctx, `
		SELECT obj_description(p.oid, 'pg_proc')
		  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		 WHERE n.nspname = 'pgbouncer' AND p.proname = 'get_auth'`,
	).Scan(&comment); err != nil {
		t.Fatalf("reading the function comment: %v", err)
	}
	if comment == nil || *comment != "installed-by-the-server" {
		t.Errorf("the server's get_auth was overwritten (comment=%v) — its security decisions were replaced with ours", comment)
	}

	// And the pooler must still be able to use it.
	poolerConn, err := pgx.Connect(ctx, tenantURI(tgt, authRole, poolerAuthPassword, db))
	if err != nil {
		t.Fatalf("pooler role cannot reach %q: %v", db, err)
	}
	defer func() { _ = poolerConn.Close(ctx) }()
	var gotUser, gotSecret string
	if err := poolerConn.QueryRow(ctx,
		"SELECT username, password FROM pgbouncer.get_auth($1)", creds.Username,
	).Scan(&gotUser, &gotSecret); err != nil {
		t.Fatalf("auth_query failed against the server's function: %v", err)
	}
	if gotUser != creds.Username || gotSecret == "" {
		t.Fatalf("auth_query returned user=%q secret empty=%t", gotUser, gotSecret == "")
	}

	// Preserving the server's function must not mean preserving its exposure.
	//
	// The planted function above keeps PostgreSQL's default EXECUTE grant to
	// PUBLIC, exactly as a CREATE FUNCTION that forgot to revoke would — and
	// this lookup returns the password verifier of every login role on the
	// cluster. Leaving the definition alone while narrowing who may call it is
	// the whole distinction between deferring to the server's authentication
	// policy and inheriting its accidents.
	t.Run("tenant cannot read the preserved lookup", func(t *testing.T) {
		conn, err := pgx.Connect(ctx, tenantURI(tgt, creds.Username, creds.Password, db))
		if err != nil {
			t.Fatalf("tenant cannot reach its own database: %v", err)
		}
		defer func() { _ = conn.Close(ctx) }()

		var u, s string
		err = conn.QueryRow(ctx,
			"SELECT username, password FROM pgbouncer.get_auth($1)", creds.Username).Scan(&u, &s)
		if err == nil {
			t.Fatal("PRIVILEGE LEAK: a tenant can read password verifiers through the preserved pgbouncer.get_auth")
		}
		msg := strings.ToLower(err.Error())
		if !strings.Contains(msg, "permission denied for schema") &&
			!strings.Contains(msg, "permission denied for function") {
			t.Fatalf("tenant was refused, but not by the grants on the lookup — got %v", err)
		}
	})
}

// TestPoolerAuthSkippedWhenRoleAbsent keeps this operator usable against a
// server with no pooler of that shape in front of it — a direct-to-primary
// deployment, or a different pooler entirely. An absent role is a fact about
// the deployment, not a failure, and the bootstrap defaults to on.
func TestPoolerAuthSkippedWhenRoleAbsent(t *testing.T) {
	tgt := testTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tgt.PoolerAuthRole = "no_such_pooler_role_here"

	p := Postgres{}
	db := testDB(t, "unpooled")
	creds, err := p.Ensure(ctx, tgt, db, "", Options{})
	if err != nil {
		t.Fatalf("provisioning against a server with no pooler role: %v", err)
	}
	cleanup(t, tgt, db)

	if err := connectAs(ctx, tgt, creds.Username, creds.Password, db); err != nil {
		t.Fatalf("tenant cannot reach its own database: %v", err)
	}

	conn, err := pgx.Connect(ctx, tenantURI(tgt, tgt.AdminUser, tgt.AdminPassword, db))
	if err != nil {
		t.Fatalf("admin connect to %q: %v", db, err)
	}
	defer func() { _ = conn.Close(ctx) }()

	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'pgbouncer')`,
	).Scan(&exists); err != nil {
		t.Fatalf("checking for the pgbouncer schema: %v", err)
	}
	if exists {
		t.Error("bootstrapped pooler auth for a role that does not exist")
	}
}
