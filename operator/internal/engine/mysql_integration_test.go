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
// Against a REAL PXC cluster, which is the run that matters before believing
// any of this — a bare container cannot show whether an assertion was quietly
// relying on what mysql:8.0 does or does not already contain:
//
//	kubectl port-forward -n <ns> svc/<claim>-haproxy 13307:3306 &
//	MIMIR_TEST_MYSQL=localhost:13307 \
//	MIMIR_TEST_MYSQL_ADMIN_USER=root \
//	MIMIR_TEST_MYSQL_ADMIN_PASSWORD="$(kubectl get secret <claim>-secrets -n <ns> \
//	  -o jsonpath='{.data.root}' | base64 -d)" \
//	  go test -tags integration ./internal/engine/... -run TestMySQL
//
// All 23 pass that way against PXC 8.0.44 through HAProxy. Note the Postgres
// side found `kubectl port-forward` unreliable for a per-connection suite — the
// node end is socat, and a reset on any single stream tears the whole forward
// down — and used two temporary NodePort Services instead. HAProxy has been
// stable here across many runs, but that is luck rather than immunity: if this
// suite starts dropping mid-run against a cluster, reach for a NodePort before
// suspecting the provisioner.
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
	"net/url"
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

	// Drop returns nil rather than an error, and the register survives.
	//
	// The error would be the louder assertion and it is the wrong one: Drop's
	// contract is "remove it if it is ours", and it already returns nil for
	// every other not-ours case. Erroring on the one name the operator is
	// certain it must not touch held the finalizer on an object that never
	// provisioned anything — a DataService named to derive this schema is
	// refused by Ensure, so it has nothing on the server, and deleting it
	// should not need a force annotation.
	//
	// What actually matters is the second assertion. "Returned nil" would also
	// be satisfied by a Drop that cheerfully destroyed the ownership register
	// for every tenant on the cluster, so the register is checked directly.
	if err := m.Drop(ctx, tgt, ownerSchema, "ns/app/uid-1"); err != nil {
		t.Fatalf("Drop on the operator's own schema should be a no-op, got: %v", err)
	}

	admin := mustAdminDB(t, tgt)
	var present int
	if err := admin.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables
		  WHERE table_schema = ? AND table_name = ?`, ownerSchema, ownerTable,
	).Scan(&present); err != nil {
		t.Fatalf("checking the register survived: %v", err)
	}
	if present != 1 {
		t.Fatal("Drop destroyed the operator's ownership register")
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

// mustAdminDB opens an admin connection for a test's own catalog checks.
func mustAdminDB(t *testing.T, tgt Target) *sql.DB {
	t.Helper()
	db, err := openMySQLAdmin(tgt)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// openMySQLURI connects using ONLY the published uri, the way a consumer that
// was handed one would.
//
// Every other helper here reassembles the connection from host, port, username,
// password and database — which is exactly how a broken `uri` stays invisible.
// On the Postgres side the published URI pointed at a pooler that could not
// authenticate anyone, and that went unnoticed for weeks precisely because no
// test ever used the field: the tests rebuilt the connection and passed.
//
// The Go driver takes a DSN rather than a URL, so the URI is parsed back into
// one. That is what a consumer's library does too, and it exercises the parts
// that can actually be wrong: the scheme, credential escaping, the host and
// port (including IPv6 brackets), the database in the path, and the TLS
// parameter.
func openMySQLURI(uri string) (*sql.DB, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("published uri does not parse: %w", err)
	}
	if u.Scheme != "mysql" {
		return nil, fmt.Errorf("published uri has scheme %q, want mysql", u.Scheme)
	}
	if u.User == nil {
		return nil, errors.New("published uri carries no credentials")
	}
	password, ok := u.User.Password()
	if !ok {
		return nil, errors.New("published uri carries no password")
	}

	cfg := mysql.NewConfig()
	cfg.User = u.User.Username()
	cfg.Passwd = password
	cfg.Net = "tcp"
	cfg.Addr = u.Host
	cfg.DBName = strings.TrimPrefix(u.Path, "/")
	if tls := u.Query().Get("tls"); tls != "" {
		cfg.TLSConfig = tls
	}
	return sql.Open("mysql", cfg.FormatDSN())
}

// TestMySQLPublishedURIConnects is the contract consumers are actually handed.
//
// `uri` is a field in the Secret that apps paste into their own configuration,
// so it has to work verbatim rather than only after being taken apart and put
// back together. Asserting it against the database it names also catches a URI
// that connects to the right server but the wrong — or no — database, which a
// bare "did it connect" check would not.
//
// The uri is documented as targeting Go consumers, since `tls=` is
// go-sql-driver's spelling. That is a reason to test it with a Go client, not a
// reason to leave the field untested: an app is told this string works.
func TestMySQLPublishedURIConnects(t *testing.T) {
	tgt := testMySQLTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	m := MySQL{}
	dbName := testMySQLDB(t, "uri_probe")
	cleanupMySQL(t, tgt, dbName)

	creds, err := m.Ensure(ctx, tgt, dbName, "", Options{Owner: "ns/uri/uid-1"})
	if err != nil {
		t.Fatalf("provisioning: %v", err)
	}

	db, err := openMySQLURI(creds.URI)
	if err != nil {
		t.Fatalf("the published uri is not usable: %v", err)
	}
	defer func() { _ = db.Close() }()

	// NullString rather than string: a uri that names no database at all
	// connects fine and returns NULL here, and scanning that into a string
	// fails with a driver-level type error that says nothing about the actual
	// problem.
	var got sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&got); err != nil {
		t.Fatalf("connecting with the published uri: %v", err)
	}
	if !got.Valid {
		t.Fatalf("the published uri connects but selects no database — %q carries no database in its path", creds.URI)
	}
	if got.String != dbName {
		t.Errorf("the published uri lands on database %q, want %q", got.String, dbName)
	}

	// And it must carry real write access, not merely connect. GRANT ALL is
	// part of the contract, so a uri that authenticates into a database the
	// tenant cannot use is still a broken uri.
	if _, err := db.ExecContext(ctx, "CREATE TABLE canary (v INT)"); err != nil {
		t.Fatalf("the published uri connects but cannot create a table: %v", err)
	}
}

// TestMySQLSteadyStateReconcileDDLBudget holds this file's opening claim to
// account.
//
// mysql.go opens by noting that on Galera every DDL statement replicates under
// Total Order Isolation — a cluster-wide stall rather than a local write — and
// that this "is a reason to keep the reconcile's steady state genuinely no-op
// rather than re-running harmless-looking DDL on a timer". Whether it does is
// measurable, and was not being measured.
//
// The budget is exact rather than an upper bound, so adding a statement to the
// steady path fails here and has to be argued for.
//
// Counted with MySQL's own per-statement counters rather than the general log,
// so the assertion is about what the server executed rather than about parsing.
func TestMySQLSteadyStateReconcileDDLBudget(t *testing.T) {
	tgt := testMySQLTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	m := MySQL{}
	const owner = "ns/steady/uid-1"
	dbName := testMySQLDB(t, "steady")
	cleanupMySQL(t, tgt, dbName)

	creds, err := m.Ensure(ctx, tgt, dbName, "", Options{Owner: owner})
	if err != nil {
		t.Fatalf("provisioning: %v", err)
	}

	admin := mustAdminDB(t, tgt)

	// ⚠️ ONLY non-parameterised statements can be budgeted this way, and that
	// constraint is not obvious enough to leave implicit.
	//
	// These `statement/sql/*` instruments count statements sent as literal text.
	// Everything this provisioner sends with placeholders — every SELECT, and
	// the ownership-register INSERT — goes over the prepared-statement protocol
	// instead and lands in `statement/com/Prepare` / `statement/com/Execute`.
	// Measured: a first provision that definitely writes an ownership row moves
	// `statement/sql/insert` by 0 and `statement/com/Execute` by 13.
	//
	// So an earlier version of this budget asserted `statement/sql/insert: 0`
	// and `statement/sql/update: 0`, and both were VACUOUSLY true — they would
	// have read zero however many rows the reconcile wrote. Removed rather than
	// corrected, because they were also measuring the wrong thing: the register
	// write is DML, replicated row-wise, not the Total Order Isolation stall
	// this budget exists to prevent.
	//
	// Do not add a `com/Execute` budget in their place either. Reading these
	// counters is itself a parameterised query, so the measurement would
	// perturb what it measures and the expected number would depend on how many
	// counters the test happens to read.
	budget := map[string]int64{
		// Drift correction that cannot be cheaply confirmed unnecessary: a
		// stored password hash cannot be compared, and a grant row's presence
		// does not prove no privilege inside it was revoked.
		"statement/sql/alter_user": 1,
		"statement/sql/grant":      1,
		// The DDL that must not recur. A reconcile that re-creates state it
		// just read pays a cluster-wide stall per tenant, every pass, for
		// nothing.
		"statement/sql/create_table": 0,
		"statement/sql/create_db":    0,
	}
	names := make([]string, 0, len(budget))
	for name := range budget {
		names = append(names, name)
	}
	before := readStatementCounters(ctx, t, admin, names)

	// A reconcile that changes nothing.
	if _, err := m.Ensure(ctx, tgt, dbName, creds.Password, Options{Owner: owner}); err != nil {
		t.Fatalf("steady-state reconcile: %v", err)
	}
	after := readStatementCounters(ctx, t, admin, names)

	for name, want := range budget {
		if delta := after[name] - before[name]; delta != want {
			t.Errorf("steady-state reconcile issued %d %s, want %d — each one is a Total Order Isolation event on Galera",
				delta, name, want)
		}
	}
}

// TestMySQLDDLBudgetCountersAreLive is the negative control for the budget
// above, and it exists because that budget already shipped two assertions that
// could never fail.
//
// A counter that stays flat proves nothing unless something is known to move
// it. This provisions a database for the FIRST time — which must issue a
// CREATE DATABASE — and fails if the harness cannot see it. Without this, a
// renamed instrument, a disabled performance_schema, or a typo in an event name
// would turn the whole budget green and silent.
func TestMySQLDDLBudgetCountersAreLive(t *testing.T) {
	tgt := testMySQLTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	m := MySQL{}
	dbName := testMySQLDB(t, "live")
	cleanupMySQL(t, tgt, dbName)

	admin := mustAdminDB(t, tgt)
	const counter = "statement/sql/create_db"
	before := readStatementCounters(ctx, t, admin, []string{counter})

	if _, err := m.Ensure(ctx, tgt, dbName, "", Options{Owner: "ns/live/uid-1"}); err != nil {
		t.Fatalf("provisioning: %v", err)
	}
	after := readStatementCounters(ctx, t, admin, []string{counter})

	if delta := after[counter] - before[counter]; delta != 1 {
		t.Fatalf("a first provision moved %s by %d, want 1 — the budget test cannot detect anything", counter, delta)
	}
}

// readStatementCounters snapshots MySQL's per-statement execution counts.
func readStatementCounters(ctx context.Context, t *testing.T, db *sql.DB, names []string) map[string]int64 {
	t.Helper()
	out := make(map[string]int64, len(names))
	for _, name := range names {
		var value int64
		// The per-statement summary rather than SHOW GLOBAL STATUS: SHOW takes
		// no placeholders, and concatenating the name into the statement to
		// work around that is the habit worth not forming. The Com_* status
		// variables are also not exposed through performance_schema.global_status
		// on 8.0, so this is the only parameterisable source for them.
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT_STAR FROM performance_schema.events_statements_summary_global_by_event_name
			  WHERE EVENT_NAME = ?`, name,
		).Scan(&value); err != nil {
			t.Fatalf("reading statement counter %q: %v", name, err)
		}
		out[name] = value
	}
	return out
}

// TestMySQLClaimDoesNotStealAFreeName covers the read-then-write window in the
// ownership claim.
//
// Ensure reads the register, decides a name is free, and then writes — two
// statements with a gap between them. Two DataServices asking for the same
// unused name can both come through that gap, and the blind upsert this used to
// do let the second one overwrite the first: both walked away believing they
// owned the database, and only one of them did.
//
// The gap is simulated rather than raced, because a real race is not reliably
// reproducible and a flaky test that proves nothing is worse than none. A
// second owner claiming a name already recorded to somebody else is exactly the
// state the losing goroutine finds itself in, and the claim has to refuse it.
func TestMySQLClaimDoesNotStealAFreeName(t *testing.T) {
	tgt := testMySQLTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	m := MySQL{}
	admin := mustAdminDB(t, tgt)
	dbName := testMySQLDB(t, "claimrace")

	if err := m.ensureOwnerRegister(ctx, admin); err != nil {
		t.Fatalf("preparing the register: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = m.deleteOwnerMarker(ctx, admin, dbName)
	})

	// First claimant wins a genuinely free name. Not reclaimable: there is no
	// row, which is the state the switch reports for an unused name.
	if err := m.claimOwnerMarker(ctx, admin, dbName, "ns/first/uid-1", false); err != nil {
		t.Fatalf("first claim on a free name: %v", err)
	}

	// Second claimant read the register before that landed, so it also believes
	// the name is free. The row now says otherwise and must win.
	err := m.claimOwnerMarker(ctx, admin, dbName, "ns/second/uid-2", false)
	if err == nil {
		t.Fatal("the second claim took a name already recorded to another owner")
	}
	var notOwned *ErrNotOwned
	if !errors.As(err, &notOwned) {
		t.Fatalf("second claim error = %v, want ErrNotOwned", err)
	}
	if notOwned.Got != "ns/first/uid-1" {
		t.Errorf("conflict names holder %q, want the first claimant", notOwned.Got)
	}

	// And the register still records the winner rather than the last writer.
	holder, err := m.databaseOwnerMarker(ctx, admin, dbName)
	if err != nil {
		t.Fatalf("reading the register: %v", err)
	}
	if holder != "ns/first/uid-1" {
		t.Errorf("owner record = %q, want the first claimant to have survived", holder)
	}

	// The same owner reclaiming its own row is a no-op, not a conflict —
	// otherwise every steady-state reconcile would fail.
	if err := m.claimOwnerMarker(ctx, admin, dbName, "ns/first/uid-1", false); err != nil {
		t.Fatalf("owner re-claiming its own record: %v", err)
	}

	// And a row the caller has established is reclaimable — recorded, but with
	// no database behind it — still yields to a new owner. Without this the
	// conditional write would strand names permanently.
	if err := m.claimOwnerMarker(ctx, admin, dbName, "ns/second/uid-2", true); err != nil {
		t.Fatalf("reclaiming a stale record: %v", err)
	}
	holder, err = m.databaseOwnerMarker(ctx, admin, dbName)
	if err != nil {
		t.Fatalf("reading the register: %v", err)
	}
	if holder != "ns/second/uid-2" {
		t.Errorf("owner record = %q, want the reclaiming owner", holder)
	}
}

// TestMySQLConflictLeavesNoOwnershipRow — a request that is about to be refused
// must not record itself as the owner on the way out.
//
// Ensure used to write the marker before validating the derived account, so a
// database name that was free but whose account belonged to somebody else left
// a row claiming a database Ensure then declined to create. Harmless-looking,
// and not: the register is what Drop consults, so the operator accumulated rows
// for databases it had never made.
func TestMySQLConflictLeavesNoOwnershipRow(t *testing.T) {
	tgt := testMySQLTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	m := MySQL{}
	admin := mustAdminDB(t, tgt)
	dbName := testMySQLDB(t, "noclaim")
	user := mysqlUserName(dbName)

	if err := m.ensureOwnerRegister(ctx, admin); err != nil {
		t.Fatalf("preparing the register: %v", err)
	}

	// A foreign account occupying the name this database would derive. No
	// ATTRIBUTE, so it reads as somebody else's however the marker is stored.
	if _, err := admin.ExecContext(ctx, fmt.Sprintf("CREATE USER %s@%s IDENTIFIED BY %s",
		quoteMySQLLiteral(user), quoteMySQLLiteral("%"), quoteMySQLLiteral("handmade")),
	); err != nil {
		t.Fatalf("planting a foreign account: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = admin.ExecContext(ctx, fmt.Sprintf("DROP USER IF EXISTS %s@%s",
			quoteMySQLLiteral(user), quoteMySQLLiteral("%")))
		_ = m.deleteOwnerMarker(ctx, admin, dbName)
	})

	_, err := m.Ensure(ctx, tgt, dbName, "", Options{Owner: "ns/app/uid-1"})
	if err == nil {
		t.Fatal("Ensure adopted a database whose account belongs to someone else")
	}
	var notOwned *ErrNotOwned
	if !errors.As(err, &notOwned) {
		t.Fatalf("Ensure error = %v, want ErrNotOwned", err)
	}

	// The point of the test: nothing was recorded.
	holder, err := m.databaseOwnerMarker(ctx, admin, dbName)
	if err != nil {
		t.Fatalf("reading the register: %v", err)
	}
	if holder != "" {
		t.Errorf("a refused request recorded itself as owner %q", holder)
	}
}

// TestMySQLDropClearsTheRecordWhenTheAccountIsForeign — the database was ours
// and is gone, so its row goes too, even though the account stays.
//
// Drop preserves an account it does not own, which is right. It used to return
// early to do so, which skipped clearing the ownership row for a database it
// had just successfully dropped: deletion reported success and left the
// operator's own bookkeeping behind.
func TestMySQLDropClearsTheRecordWhenTheAccountIsForeign(t *testing.T) {
	tgt := testMySQLTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	m := MySQL{}
	admin := mustAdminDB(t, tgt)
	dbName := testMySQLDB(t, "foreignacct")
	owner := "ns/app/uid-1"
	user := mysqlUserName(dbName)

	if _, err := m.Ensure(ctx, tgt, dbName, "", Options{Owner: owner}); err != nil {
		t.Fatalf("provisioning: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = admin.ExecContext(ctx, fmt.Sprintf("DROP USER IF EXISTS %s@%s",
			quoteMySQLLiteral(user), quoteMySQLLiteral("%")))
		_ = m.deleteOwnerMarker(ctx, admin, dbName)
	})

	// Repoint the account's marker at somebody else, leaving the database's own
	// record ours. That is the split Drop has to handle: our database, not our
	// account.
	if _, err := admin.ExecContext(ctx, fmt.Sprintf("ALTER USER %s@%s ATTRIBUTE %s",
		quoteMySQLLiteral(user), quoteMySQLLiteral("%"),
		quoteMySQLLiteral(ownerAttributeJSON("ns/other/uid-9"))),
	); err != nil {
		t.Fatalf("repointing the account marker: %v", err)
	}

	if err := m.Drop(ctx, tgt, dbName, owner); err != nil {
		t.Fatalf("dropping: %v", err)
	}

	// The database is gone.
	exists, err := m.databaseExists(ctx, admin, dbName)
	if err != nil {
		t.Fatalf("checking the database: %v", err)
	}
	if exists {
		t.Error("Drop left the database behind")
	}

	// The account is NOT, because it was not ours to remove.
	stillThere, err := m.userExists(ctx, admin, user)
	if err != nil {
		t.Fatalf("checking the account: %v", err)
	}
	if !stillThere {
		t.Error("Drop removed an account belonging to another owner")
	}

	// And the row is gone, which is the regression.
	holder, err := m.databaseOwnerMarker(ctx, admin, dbName)
	if err != nil {
		t.Fatalf("reading the register: %v", err)
	}
	if holder != "" {
		t.Errorf("Drop left ownership row %q behind for a database it deleted", holder)
	}
}
