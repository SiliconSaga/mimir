//go:build integration

// Integration tests for the MySQL provisioner against a real server.
//
// The unit tests cover identifier quoting, wildcard escaping and user-name
// derivation, which is where an injection or a collision would live. They
// cannot cover the thing that decides whether a shared cluster is safe:
// whether MySQL really refuses a cross-tenant query given the grants this code
// writes. That is a property of the server, not of this code, so it needs a
// server.
//
// Run against a throwaway container:
//
//	docker run -d --name mimir-mysqltest -e MYSQL_ROOT_PASSWORD=testadminpw \
//	  -p 13306:3306 mysql:8.0
//	MIMIR_TEST_MYSQL=localhost:13306 go test -tags integration ./internal/engine/...
//
// Percona XtraDB Cluster works equally well and is what the platform actually
// runs; plain mysql:8.0 starts faster and shares the SQL surface this exercises.
//
// Skipped unless MIMIR_TEST_MYSQL is set, so the default `go test ./...` stays
// hermetic and CI does not need a database.
package engine

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func testMySQLDB(t *testing.T, stem string) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return stem + "_" + hex.EncodeToString(b[:])
}

func testMySQLTarget(t *testing.T) Target {
	t.Helper()
	addr := os.Getenv("MIMIR_TEST_MYSQL")
	if addr == "" {
		t.Skip("MIMIR_TEST_MYSQL not set")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("MIMIR_TEST_MYSQL must be host:port, got %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("bad port in MIMIR_TEST_MYSQL: %v", err)
	}
	if port < 1 || port > 65535 {
		t.Fatalf("port %d in MIMIR_TEST_MYSQL out of range", port)
	}
	admin := os.Getenv("MIMIR_TEST_MYSQL_ADMIN_USER")
	if admin == "" {
		admin = "root"
	}
	pw := os.Getenv("MIMIR_TEST_MYSQL_ADMIN_PASSWORD")
	if pw == "" {
		pw = "testadminpw"
	}
	return Target{
		Host: host, Port: int32(port),
		AdminHost: host, AdminPort: int32(port),
		AdminUser: admin, AdminPassword: pw,
		AdminDatabase: "mysql",
		// The throwaway container serves plaintext. Percona clusters require
		// TLS, which the provisioner handles via Target.TLS.
		TLS: os.Getenv("MIMIR_TEST_MYSQL_TLS") == "true",
	}
}

func cleanupMySQL(t *testing.T, tgt Target, database string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := (MySQL{}).Drop(ctx, tgt, database, ""); err != nil {
			t.Errorf("cleanup: dropping %q left state behind: %v", database, err)
		}
	})
}

// openMySQL connects with an arbitrary credential, which is how a tenant's own
// connection is simulated.
func openMySQL(tgt Target, user, password, database string) (*sql.DB, error) {
	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = password
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(tgt.AdminHost, strconv.Itoa(int(tgt.AdminPort)))
	cfg.DBName = database
	if tgt.TLS {
		cfg.TLSConfig = "skip-verify"
	}
	return sql.Open("mysql", cfg.FormatDSN())
}

// queryAs runs a trivial read against a database as the given credential.
func queryAs(ctx context.Context, tgt Target, user, password, database string) error {
	db, err := openMySQL(tgt, user, password, database)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	var one int
	return db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

// isAccessDenied reports whether the error is MySQL refusing access, rather
// than any other failure.
//
// The Postgres suite greps for permission-denied text for a reason: a wrong
// hostname also exits non-zero and would otherwise read as a pass. MySQL gives
// us something better than text — error numbers 1044 (access denied for user to
// database) and 1045 (access denied, bad credentials) — so match on those and
// fall back to text only for drivers that lose the code.
func isAccessDenied(err error) bool {
	if err == nil {
		return false
	}
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		return myErr.Number == 1044 || myErr.Number == 1045 || myErr.Number == 1142
	}
	return strings.Contains(strings.ToLower(err.Error()), "access denied")
}

// TestMySQLTenantIsolation is the load-bearing one.
//
// Provisions two tenants and asserts each is refused by the other's database,
// insisting on the RIGHT failure so a connection error cannot masquerade as
// isolation.
func TestMySQLTenantIsolation(t *testing.T) {
	tgt := testMySQLTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	m := MySQL{}
	alphaDB := testMySQLDB(t, "tenant_alpha")
	betaDB := testMySQLDB(t, "tenant_beta")

	alpha, err := m.Ensure(ctx, tgt, alphaDB, "", Options{})
	if err != nil {
		t.Fatalf("provisioning tenant_alpha: %v", err)
	}
	cleanupMySQL(t, tgt, alphaDB)

	beta, err := m.Ensure(ctx, tgt, betaDB, "", Options{})
	if err != nil {
		t.Fatalf("provisioning tenant_beta: %v", err)
	}
	cleanupMySQL(t, tgt, betaDB)

	// Each tenant reaches its own database. Without this the refusals below
	// could be caused by the credentials simply not working at all.
	if err := queryAs(ctx, tgt, alpha.Username, alpha.Password, alphaDB); err != nil {
		t.Fatalf("alpha cannot reach its own database: %v", err)
	}
	if err := queryAs(ctx, tgt, beta.Username, beta.Password, betaDB); err != nil {
		t.Fatalf("beta cannot reach its own database: %v", err)
	}

	// And is refused by the other's.
	if err := queryAs(ctx, tgt, alpha.Username, alpha.Password, betaDB); !isAccessDenied(err) {
		t.Fatalf("alpha reaching beta's database: got %v, want an access-denied error", err)
	}
	if err := queryAs(ctx, tgt, beta.Username, beta.Password, alphaDB); !isAccessDenied(err) {
		t.Fatalf("beta reaching alpha's database: got %v, want an access-denied error", err)
	}
}

// TestMySQLGrantWildcardDoesNotLeak is the MySQL-specific hazard, and the
// negative control for it.
//
// In a GRANT the database part is a LIKE pattern. `GRANT ALL ON app_one.*`
// without escaping also grants on `appXone` — a database that may not exist
// yet, and may later belong to someone else. DerivePhysicalName joins namespace
// and name with an underscore, so essentially every real name is exposed to
// this.
//
// The test provisions a tenant whose name contains an underscore, then creates
// a SECOND database matching that name as a pattern, and asserts the first
// tenant cannot touch it. The negative control below proves the test can fail.
func TestMySQLGrantWildcardDoesNotLeak(t *testing.T) {
	tgt := testMySQLTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	m := MySQL{}
	// Deliberately underscore-bearing, matching what DerivePhysicalName emits.
	suffix := testMySQLDB(t, "")
	tenantDB := "wild_a" + suffix
	// Differs from tenantDB only where the underscore is, so it matches
	// tenantDB read as a LIKE pattern.
	decoyDB := "wildXa" + suffix

	tenant, err := m.Ensure(ctx, tgt, tenantDB, "", Options{})
	if err != nil {
		t.Fatalf("provisioning tenant: %v", err)
	}
	cleanupMySQL(t, tgt, tenantDB)

	admin, err := openMySQL(tgt, tgt.AdminUser, tgt.AdminPassword, tgt.AdminDatabase)
	if err != nil {
		t.Fatalf("admin connection: %v", err)
	}
	defer func() { _ = admin.Close() }()

	if _, err := admin.ExecContext(ctx,
		fmt.Sprintf("CREATE DATABASE %s", quoteMySQLIdentifier(decoyDB))); err != nil {
		t.Fatalf("create decoy database: %v", err)
	}
	t.Cleanup(func() {
		// A FRESH connection, not the one deferred-closed above. t.Cleanup runs
		// after the test function returns, so every defer has already fired and
		// `admin` is closed by then — the first version of this failed with
		// "sql: database is closed" and left the decoy database behind.
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := openMySQL(tgt, tgt.AdminUser, tgt.AdminPassword, tgt.AdminDatabase)
		if err != nil {
			t.Errorf("cleanup: reconnecting to drop decoy %q: %v", decoyDB, err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, err := conn.ExecContext(c,
			fmt.Sprintf("DROP DATABASE IF EXISTS %s", quoteMySQLIdentifier(decoyDB))); err != nil {
			t.Errorf("cleanup: dropping decoy %q: %v", decoyDB, err)
		}
	})

	// NEGATIVE CONTROL. Grant the same tenant an UNESCAPED pattern and confirm
	// the leak is real on this server — otherwise the assertion that follows
	// proves nothing, because MySQL might simply not behave the way the comment
	// on grantDatabasePattern claims.
	unescaped := "`" + strings.ReplaceAll(tenantDB, "`", "``") + "`"
	if _, err := admin.ExecContext(ctx,
		fmt.Sprintf("GRANT ALL PRIVILEGES ON %s.* TO %s@%s",
			unescaped, quoteMySQLLiteral(tenant.Username), quoteMySQLLiteral("%"))); err != nil {
		t.Fatalf("negative control grant: %v", err)
	}
	if err := queryAs(ctx, tgt, tenant.Username, tenant.Password, decoyDB); err != nil {
		t.Fatalf("negative control failed: an UNESCAPED grant should have leaked into %q, but got %v — "+
			"this server does not treat the grant database as a pattern, so the escaped assertion below is vacuous",
			decoyDB, err)
	}

	// Now revoke the unescaped grant and re-run the real provisioner, which
	// writes the escaped form. The leak must be gone.
	if _, err := admin.ExecContext(ctx,
		fmt.Sprintf("REVOKE ALL PRIVILEGES ON %s.* FROM %s@%s",
			unescaped, quoteMySQLLiteral(tenant.Username), quoteMySQLLiteral("%"))); err != nil {
		t.Fatalf("revoking negative control grant: %v", err)
	}
	if _, err := m.Ensure(ctx, tgt, tenantDB, tenant.Password, Options{}); err != nil {
		t.Fatalf("re-provisioning tenant: %v", err)
	}

	if err := queryAs(ctx, tgt, tenant.Username, tenant.Password, decoyDB); !isAccessDenied(err) {
		t.Fatalf("escaped grant still reaches %q: got %v, want access denied", decoyDB, err)
	}
	// And the tenant still reaches its own database.
	if err := queryAs(ctx, tgt, tenant.Username, tenant.Password, tenantDB); err != nil {
		t.Fatalf("tenant lost access to its own database: %v", err)
	}
}

// TestMySQLIdempotentAndPasswordStable — Ensure runs on every reconcile, so the
// steady state must be a no-op. A password that changed per pass would rotate
// the credential out from under every consumer that cached it.
func TestMySQLIdempotentAndPasswordStable(t *testing.T) {
	tgt := testMySQLTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	m := MySQL{}
	dbName := testMySQLDB(t, "idem")

	first, err := m.Ensure(ctx, tgt, dbName, "", Options{Owner: "ns/app/uid-1"})
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	cleanupMySQL(t, tgt, dbName)

	second, err := m.Ensure(ctx, tgt, dbName, first.Password, Options{Owner: "ns/app/uid-1"})
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if second.Password != first.Password {
		t.Error("password changed between reconciles; consumers would break")
	}
	if err := queryAs(ctx, tgt, second.Username, second.Password, dbName); err != nil {
		t.Fatalf("credentials stopped working after the second reconcile: %v", err)
	}
}

// TestMySQLOwnershipRefusesForeignDatabase — the marker is what stops a second
// DataService resolving to the same physical name from adopting the first
// tenant's data and resetting its password.
func TestMySQLOwnershipRefusesForeignDatabase(t *testing.T) {
	tgt := testMySQLTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	m := MySQL{}
	dbName := testMySQLDB(t, "owned")

	if _, err := m.Ensure(ctx, tgt, dbName, "", Options{Owner: "ns/first/uid-1"}); err != nil {
		t.Fatalf("first owner Ensure: %v", err)
	}
	cleanupMySQL(t, tgt, dbName)

	_, err := m.Ensure(ctx, tgt, dbName, "", Options{Owner: "ns/second/uid-2"})
	var notOwned *ErrNotOwned
	if !errors.As(err, &notOwned) {
		t.Fatalf("second owner: got %v, want ErrNotOwned", err)
	}
}

// TestMySQLDropRefusesForeignDatabase — Drop is called from a finalizer that
// exists even on an object that LOST an ownership conflict, so dropping by name
// alone would destroy the legitimate tenant's database.
func TestMySQLDropRefusesForeignDatabase(t *testing.T) {
	tgt := testMySQLTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	m := MySQL{}
	dbName := testMySQLDB(t, "keepme")

	creds, err := m.Ensure(ctx, tgt, dbName, "", Options{Owner: "ns/owner/uid-1"})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	cleanupMySQL(t, tgt, dbName)

	// A different owner asks for it to be dropped. Nothing should happen.
	if err := m.Drop(ctx, tgt, dbName, "ns/impostor/uid-2"); err != nil {
		t.Fatalf("Drop with a foreign owner should be a no-op, got: %v", err)
	}
	if err := queryAs(ctx, tgt, creds.Username, creds.Password, dbName); err != nil {
		t.Fatalf("the legitimate tenant's database was destroyed by a foreign Drop: %v", err)
	}
}

// TestMySQLDropThenReprovision — deleting a DataService and declaring it again
// must work, which means Drop has to leave nothing behind that Ensure trips on.
func TestMySQLDropThenReprovision(t *testing.T) {
	tgt := testMySQLTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	m := MySQL{}
	dbName := testMySQLDB(t, "recycle")

	if _, err := m.Ensure(ctx, tgt, dbName, "", Options{Owner: "ns/app/uid-1"}); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	if err := m.Drop(ctx, tgt, dbName, "ns/app/uid-1"); err != nil {
		t.Fatalf("Drop: %v", err)
	}

	// A new UID, as a recreated object would have.
	creds, err := m.Ensure(ctx, tgt, dbName, "", Options{Owner: "ns/app/uid-2"})
	if err != nil {
		t.Fatalf("re-provision after drop: %v", err)
	}
	cleanupMySQL(t, tgt, dbName)

	if err := queryAs(ctx, tgt, creds.Username, creds.Password, dbName); err != nil {
		t.Fatalf("re-provisioned credentials do not work: %v", err)
	}
}

// TestMySQLLongDatabaseNameProvisions covers the 32-character user limit
// against a real server, where exceeding it is ERROR 1470 rather than anything
// this code could detect on its own.
func TestMySQLLongDatabaseNameProvisions(t *testing.T) {
	tgt := testMySQLTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	m := MySQL{}
	// 63 characters, the longest the API permits and DerivePhysicalName emits.
	// testMySQLDB(t, "") returns "_" plus 8 hex characters; the leading
	// underscore is stripped, leaving 8 to make the name unique per run.
	const uniqueLen = 8
	dbName := strings.Repeat("l", 63-uniqueLen) + testMySQLDB(t, "")[1:]
	if len(dbName) != 63 {
		t.Fatalf("test set-up produced a %d-character name, want 63", len(dbName))
	}

	creds, err := m.Ensure(ctx, tgt, dbName, "", Options{})
	if err != nil {
		t.Fatalf("provisioning a 63-character database: %v", err)
	}
	cleanupMySQL(t, tgt, dbName)

	if len(creds.Username) > mysqlMaxUserLength {
		t.Errorf("published username %q is %d characters, over MySQL's limit", creds.Username, len(creds.Username))
	}
	if err := queryAs(ctx, tgt, creds.Username, creds.Password, dbName); err != nil {
		t.Fatalf("credentials for a long-named database do not work: %v", err)
	}
}
