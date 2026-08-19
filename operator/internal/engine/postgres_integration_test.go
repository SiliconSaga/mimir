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
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func testTarget(t *testing.T) Target {
	t.Helper()
	addr := os.Getenv("MIMIR_TEST_PG")
	if addr == "" {
		t.Skip("MIMIR_TEST_PG not set")
	}
	host, portStr, ok := strings.Cut(addr, ":")
	if !ok {
		t.Fatalf("MIMIR_TEST_PG must be host:port, got %q", addr)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("bad port in MIMIR_TEST_PG: %v", err)
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

// connectAs opens a connection with an arbitrary credential, which is how a
// tenant's own connection is simulated.
func connectAs(ctx context.Context, tgt Target, user, password, database string) error {
	uri := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		user, password, tgt.AdminHost, tgt.AdminPort, database)
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

	alpha, err := p.Ensure(ctx, tgt, "tenant_alpha", "", Options{})
	if err != nil {
		t.Fatalf("provisioning tenant_alpha: %v", err)
	}
	t.Cleanup(func() { _ = p.Drop(context.Background(), tgt, "tenant_alpha") })

	beta, err := p.Ensure(ctx, tgt, "tenant_beta", "", Options{})
	if err != nil {
		t.Fatalf("provisioning tenant_beta: %v", err)
	}
	t.Cleanup(func() { _ = p.Drop(context.Background(), tgt, "tenant_beta") })

	// Each tenant reaches its own database.
	if err := connectAs(ctx, tgt, alpha.Username, alpha.Password, "tenant_alpha"); err != nil {
		t.Fatalf("alpha cannot reach its own database: %v", err)
	}
	if err := connectAs(ctx, tgt, beta.Username, beta.Password, "tenant_beta"); err != nil {
		t.Fatalf("beta cannot reach its own database: %v", err)
	}

	// Neither reaches the other's, and for the right reason.
	for _, tc := range []struct {
		name, user, password, database string
	}{
		{"alpha into beta", alpha.Username, alpha.Password, "tenant_beta"},
		{"beta into alpha", beta.Username, beta.Password, "tenant_alpha"},
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

	first, err := p.Ensure(ctx, tgt, "tenant_idem", "", Options{})
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	t.Cleanup(func() { _ = p.Drop(context.Background(), tgt, "tenant_idem") })

	second, err := p.Ensure(ctx, tgt, "tenant_idem", first.Password, Options{})
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
	if err := connectAs(ctx, tgt, second.Username, second.Password, "tenant_idem"); err != nil {
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

	creds, err := p.Ensure(ctx, tgt, "tenant_drop", "", Options{})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := p.Drop(ctx, tgt, "tenant_drop"); err != nil {
		t.Fatalf("Drop: %v", err)
	}

	admin := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		tgt.AdminUser, tgt.AdminPassword, tgt.AdminHost, tgt.AdminPort, tgt.AdminDatabase)
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer conn.Close(ctx)

	var n int
	if err := conn.QueryRow(ctx,
		"SELECT count(*) FROM pg_database WHERE datname = $1", "tenant_drop").Scan(&n); err != nil {
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
	if _, err := p.Ensure(ctx, tgt, "tenant_drop", "", Options{}); err != nil {
		t.Fatalf("re-provisioning after Drop: %v", err)
	}
	t.Cleanup(func() { _ = p.Drop(context.Background(), tgt, "tenant_drop") })
}
