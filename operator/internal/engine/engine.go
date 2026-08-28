// Package engine holds the per-engine provisioning logic.
//
// One request kind, one controller, and a Provisioner per engine behind this
// interface. Adding MySQL or MongoDB is implementing this interface and
// registering it — the DataService API does not change.
package engine

import (
	"context"
	"fmt"
	"time"

	mimirv1alpha1 "github.com/SiliconSaga/mimir/operator/api/v1alpha1"
)

// Connection deadlines, shared by every engine.
//
// Neither driver sets one by default, and controller-runtime gives Reconcile no
// deadline either, so without these an unresponsive server holds a reconcile
// worker forever. The worker pool is small, so that is not one stuck tenant —
// it is the controller stopping, for every engine, while looking healthy.
//
// Generous rather than tight: these are a backstop against a server that is
// gone, not a latency budget. A shared cluster under load can be slow to answer
// DDL without anything being wrong, and a timeout that fires then would turn
// contention into a reconcile error.
const (
	dialTimeout = 10 * time.Second
	ioTimeout   = 30 * time.Second
)

// Target describes the server a database is being vended inside.
//
// Admin and consumer endpoints are deliberately separate. DDL such as
// CREATE DATABASE cannot run inside a transaction block, so it must reach the
// primary directly — a pooler in transaction mode rejects it. Consumers are
// handed the pooler, which is the whole reason one is deployed.
type Target struct {
	// Host and Port are what CONSUMERS connect to — the pooler where one exists.
	Host string
	Port int32

	// AdminHost and AdminPort are where DDL runs: the primary, not the pooler.
	AdminHost string
	AdminPort int32

	// AdminUser and AdminPassword can create databases and roles.
	AdminUser     string
	AdminPassword string

	// AdminDatabase is the database to connect to in order to administer
	// others. Postgres has no "no database" connection, so DDL that creates a
	// database has to run from somewhere else.
	AdminDatabase string

	// TLS is whether the server requires an encrypted connection. Percona and
	// Crunchy clusters serve hostssl only, and connecting with sslmode=disable
	// is rejected outright rather than downgraded.
	TLS bool

	// PoolerAuthRole is the role the pooler in front of Host authenticates AS
	// when it looks a client's password up. Empty means "no bootstrap needed".
	//
	// This exists because a pooler does not verify passwords itself. Percona's
	// (and Crunchy's) pgBouncer runs an auth_query — by default
	// `SELECT username, password FROM pgbouncer.get_auth($1)` — by connecting
	// as this role INTO THE DATABASE THE CLIENT ASKED FOR. A database this
	// operator vends has neither the CONNECT grant (revoked from PUBLIC on
	// purpose) nor that function, so the lookup fails and every client is
	// refused. See Postgres.ensurePoolerAuth for what is installed and why.
	PoolerAuthRole string
}

// Credentials are what gets written into the consuming namespace's Secret.
type Credentials struct {
	Host     string
	Port     int32
	Database string
	Username string
	Password string
	// URI is a ready-assembled connection string, so apps that want one do not
	// each reassemble it slightly differently.
	URI string
}

// Provisioner vends a database inside an existing server for one engine.
//
// Implementations must be idempotent: Ensure runs on every reconcile, and the
// common case is that everything already exists and nothing changes.
type Provisioner interface {
	// Engine is the enum value this provisioner handles.
	Engine() mimirv1alpha1.Engine

	// Ensure makes the database, its owning identity and its grants exist, and
	// returns the credentials to publish. The password is generated on first
	// call; on later calls the existing one is passed in as current so the
	// value stays stable — rotating a password on every reconcile would break
	// every consumer that cached it.
	Ensure(ctx context.Context, t Target, database string, current string, opts Options) (Credentials, error)

	// Drop removes what Ensure created, but only what the given owner owns.
	//
	// The owner is checked against the marker recorded at creation, and a
	// mismatch means "nothing here is mine" rather than an error. Without it,
	// deleting a DataService that lost an ownership conflict would drop the
	// database belonging to the one that won. An empty owner skips the check.
	Drop(ctx context.Context, t Target, database, owner string) error
}

// Options carries the engine-specific extras from the spec.
type Options struct {
	// Extensions to enable. PostgreSQL only; ignored elsewhere.
	Extensions []string

	// Owner identifies the DataService this database belongs to, as
	// "namespace/name". It is recorded on the database at creation and checked
	// on every later pass, so a second DataService that resolves to the same
	// physical name is refused instead of silently adopting the first one's
	// data and resetting its password.
	Owner string
}

// ErrNotOwned is returned when a database exists but belongs to a different
// DataService — or to something that predates the operator entirely.
//
// Never resolve this automatically. Adopting the database would hand one
// tenant another's data, and dropping it would destroy data the operator did
// not create. It needs a human.
type ErrNotOwned struct {
	Database string
	Want     string
	Got      string
}

func (e *ErrNotOwned) Error() string {
	got := e.Got
	if got == "" {
		got = "no owner marker (created outside the operator)"
	}
	return fmt.Sprintf("database %q belongs to %s, not %s — refusing to touch it",
		e.Database, got, e.Want)
}

// Registry maps engines to their provisioners.
type Registry struct {
	provisioners map[mimirv1alpha1.Engine]Provisioner
}

// NewRegistry builds a registry from the given provisioners.
func NewRegistry(ps ...Provisioner) *Registry {
	r := &Registry{provisioners: map[mimirv1alpha1.Engine]Provisioner{}}
	for _, p := range ps {
		r.provisioners[p.Engine()] = p
	}
	return r
}

// For returns the provisioner for an engine, and whether one is registered.
// A missing entry is a real answer, not an error: the CRD enum accepts three
// engines and v1 only implements one, so "not built yet" has to be reportable
// on the object rather than crashing the controller.
func (r *Registry) For(e mimirv1alpha1.Engine) (Provisioner, bool) {
	p, ok := r.provisioners[e]
	return p, ok
}

// Engines lists the registered engines, for logging at startup.
func (r *Registry) Engines() []string {
	out := make([]string, 0, len(r.provisioners))
	for e := range r.provisioners {
		out = append(out, string(e))
	}
	return out
}
