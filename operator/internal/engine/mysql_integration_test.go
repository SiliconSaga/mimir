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
//
// Uses the CONSUMER endpoint (Host/Port), not the admin one, because that is
// what a tenant is actually handed in its Secret. They are the same address in
// this harness, so nothing changes today — but pointing at AdminHost would mean
// a deployment that genuinely splits the two could break every consumer while
// these tests stayed green.
func openMySQL(tgt Target, user, password, database string) (*sql.DB, error) {
	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = password
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(tgt.Host, strconv.Itoa(int(tgt.Port)))
	cfg.DBName = database
	if tgt.TLS {
		cfg.TLSConfig = "skip-verify"
	}
	return sql.Open("mysql", cfg.FormatDSN())
}

// openMySQLAdmin connects to the ADMIN endpoint, for the direct catalog and
// GRANT manipulation a test does on its own behalf. Kept separate from
// openMySQL so the consumer/admin split stays visible in the tests rather than
// resting on the two happening to be the same address here.
func openMySQLAdmin(tgt Target) (*sql.DB, error) {
	cfg := mysql.NewConfig()
	cfg.User = tgt.AdminUser
	cfg.Passwd = tgt.AdminPassword
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(tgt.AdminHost, strconv.Itoa(int(tgt.AdminPort)))
	cfg.DBName = tgt.AdminDatabase
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

	admin, err := openMySQLAdmin(tgt)
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
		conn, err := openMySQLAdmin(tgt)
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
	// The conflict must be reported against the DATABASE the caller asked for.
	// This message goes verbatim into status.conditions, and for a long name
	// the MySQL account is a truncated hash the user has never seen — naming it
	// as the thing that conflicts sends them looking for a string that appears
	// nowhere in their manifest.
	if notOwned.Database != dbName {
		t.Errorf("conflict reported against %q, want the requested database %q",
			notOwned.Database, dbName)
	}
}

// TestMySQLOwnershipConflictOnUserNamesTheDatabase covers the same requirement
// on the OTHER path into ErrNotOwned: an account that is not ours while the
// database itself is absent, which is what a partially-cleaned-up tenant leaves
// behind.
func TestMySQLOwnershipConflictOnUserNamesTheDatabase(t *testing.T) {
	tgt := testMySQLTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	m := MySQL{}
	dbName := testMySQLDB(t, "useronly")

	if _, err := m.Ensure(ctx, tgt, dbName, "", Options{Owner: "ns/first/uid-1"}); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := m.Drop(c, tgt, dbName, ""); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	// Drop the database but leave the account, so the next Ensure reaches the
	// user-ownership branch rather than the database one.
	admin, err := openMySQLAdmin(tgt)
	if err != nil {
		t.Fatalf("admin connection: %v", err)
	}
	defer func() { _ = admin.Close() }()
	if _, err := admin.ExecContext(ctx,
		fmt.Sprintf("DROP DATABASE IF EXISTS %s", quoteMySQLIdentifier(dbName))); err != nil {
		t.Fatalf("dropping database while keeping the account: %v", err)
	}

	_, err = m.Ensure(ctx, tgt, dbName, "", Options{Owner: "ns/second/uid-2"})
	var notOwned *ErrNotOwned
	if !errors.As(err, &notOwned) {
		t.Fatalf("second owner: got %v, want ErrNotOwned", err)
	}
	if notOwned.Database != dbName {
		t.Errorf("conflict reported against %q, want the requested database %q",
			notOwned.Database, dbName)
	}
	// The account should still be named somewhere, since that is the actual
	// obstruction — just not in the "database X belongs to" slot.
	if !strings.Contains(err.Error(), mysqlUserName(dbName)) {
		t.Errorf("error %q should name the conflicting account %q", err, mysqlUserName(dbName))
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

// TestMySQLTenantCannotTamperWithOwnership is why the ownership register lives
// in the operator's own schema rather than inside the tenant's database.
//
// `GRANT ALL PRIVILEGES ON <db>.*` reaches every table in that database. With
// the marker stored there, a tenant could rewrite it — and the damaging
// direction is not the obvious one: pointing it elsewhere makes Drop return nil
// without deleting, so removing the DataService would leave the database and
// its data behind, orphaned.
func TestMySQLTenantCannotTamperWithOwnership(t *testing.T) {
	tgt := testMySQLTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	m := MySQL{}
	dbName := testMySQLDB(t, "tamper")
	owner := "ns/app/uid-1"

	creds, err := m.Ensure(ctx, tgt, dbName, "", Options{Owner: owner})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	cleanupMySQL(t, tgt, dbName)

	// As the TENANT, try to reach the register at all.
	tenant, err := openMySQL(tgt, creds.Username, creds.Password, dbName)
	if err != nil {
		t.Fatalf("tenant connection: %v", err)
	}
	defer func() { _ = tenant.Close() }()

	_, err = tenant.ExecContext(ctx, fmt.Sprintf(
		"UPDATE %s.%s SET owner = 'ns/impostor/uid-9' WHERE database_name = ?",
		quoteMySQLIdentifier(ownerSchema), quoteMySQLIdentifier(ownerTable)), dbName)
	if err == nil {
		t.Fatal("the tenant rewrote its own ownership record; the register is reachable from the tenant's grant")
	}
	if !isAccessDenied(err) {
		t.Fatalf("expected an access-denied error reaching the register, got: %v", err)
	}

	// Belt and braces: the register still says what it should, and Drop still
	// removes the database.
	admin, err := openMySQLAdmin(tgt)
	if err != nil {
		t.Fatalf("admin connection: %v", err)
	}
	defer func() { _ = admin.Close() }()

	got, err := m.databaseOwnerMarker(ctx, admin, dbName)
	if err != nil {
		t.Fatalf("reading the register: %v", err)
	}
	if got != owner {
		t.Errorf("owner record = %q, want %q", got, owner)
	}
}

// TestMySQLRefusesTheOperatorsOwnSchema — ValidateIdentifier accepts
// "mimir_dataservice", and a DataService in namespace `mimir` named
// `dataservice` derives exactly that. Provisioning it would hand that tenant
// GRANT ALL over every other tenant's ownership record.
func TestMySQLRefusesTheOperatorsOwnSchema(t *testing.T) {
	tgt := testMySQLTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	m := MySQL{}
	if _, err := m.Ensure(ctx, tgt, ownerSchema, "", Options{Owner: "ns/app/uid-1"}); err == nil {
		t.Fatal("Ensure provisioned the operator's own register schema as a tenant database")
	}
	if err := m.Drop(ctx, tgt, ownerSchema, "ns/app/uid-1"); err == nil {
		t.Fatal("Drop accepted the operator's own register schema")
	}
}

// TestMySQLStaleOwnerRecordIsReclaimable — moving the register out of the
// tenant database means it can outlive a hand-dropped database. A record with
// nothing behind it must not strand the name forever.
func TestMySQLStaleOwnerRecordIsReclaimable(t *testing.T) {
	tgt := testMySQLTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	m := MySQL{}
	dbName := testMySQLDB(t, "stale")

	if _, err := m.Ensure(ctx, tgt, dbName, "", Options{Owner: "ns/first/uid-1"}); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}

	// Remove the database AND its account behind the operator's back, leaving
	// only the register row — a hand cleanup that missed the register.
	//
	// The account has to go too. A surviving account is refused separately, and
	// deliberately: adopting one by name would inherit whatever it already
	// holds, which is the privilege-escalation-in-a-Secret the marker exists to
	// prevent. TestMySQLOwnershipConflictOnUserNamesTheDatabase covers that
	// case; this one is about the register row alone.
	admin, err := openMySQLAdmin(tgt)
	if err != nil {
		t.Fatalf("admin connection: %v", err)
	}
	defer func() { _ = admin.Close() }()
	if _, err := admin.ExecContext(ctx,
		fmt.Sprintf("DROP DATABASE IF EXISTS %s", quoteMySQLIdentifier(dbName))); err != nil {
		t.Fatalf("dropping the database by hand: %v", err)
	}
	if _, err := admin.ExecContext(ctx, fmt.Sprintf("DROP USER IF EXISTS %s@%s",
		quoteMySQLLiteral(mysqlUserName(dbName)), quoteMySQLLiteral("%"))); err != nil {
		t.Fatalf("dropping the account by hand: %v", err)
	}

	// A different owner may now claim the name: there is no data to protect.
	creds, err := m.Ensure(ctx, tgt, dbName, "", Options{Owner: "ns/second/uid-2"})
	if err != nil {
		t.Fatalf("reclaiming a name whose database is gone: %v", err)
	}
	cleanupMySQL(t, tgt, dbName)

	if err := queryAs(ctx, tgt, creds.Username, creds.Password, dbName); err != nil {
		t.Fatalf("reclaimed credentials do not work: %v", err)
	}
	got, err := m.databaseOwnerMarker(ctx, admin, dbName)
	if err != nil {
		t.Fatalf("reading the register: %v", err)
	}
	if got != "ns/second/uid-2" {
		t.Errorf("owner record = %q, want the reclaiming owner", got)
	}
}

// TestMySQLDropClearsTheOwnerRecord — the register outlives the database, so a
// row left behind would make the name look permanently taken.
func TestMySQLDropClearsTheOwnerRecord(t *testing.T) {
	tgt := testMySQLTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	m := MySQL{}
	dbName := testMySQLDB(t, "cleared")
	owner := "ns/app/uid-1"

	if _, err := m.Ensure(ctx, tgt, dbName, "", Options{Owner: owner}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := m.Drop(ctx, tgt, dbName, owner); err != nil {
		t.Fatalf("Drop: %v", err)
	}

	admin, err := openMySQLAdmin(tgt)
	if err != nil {
		t.Fatalf("admin connection: %v", err)
	}
	defer func() { _ = admin.Close() }()

	got, err := m.databaseOwnerMarker(ctx, admin, dbName)
	if err != nil {
		t.Fatalf("reading the register: %v", err)
	}
	if got != "" {
		t.Errorf("owner record survived Drop as %q; the name would look taken forever", got)
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
