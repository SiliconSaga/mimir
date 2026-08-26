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
