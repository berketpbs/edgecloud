//go:build integration
// +build integration

// Package migrations_test exercises every migration in this directory
// against a real Postgres via testcontainers, proving two contracts:
//
//  1. Forward apply: every *.sql file in this directory parses under
//     rubenv/sql-migrate and applies cleanly to a fresh database.
//     Catches malformed markers, CREATE INDEX CONCURRENTLY in a
//     default-wrapped transaction (migrate.go:540-548), and any
//     regressions in the SQL itself that the sqlmock-based repository
//     tests silently allow.
//
//  2. Round-trip reversibility: rolling all the way back and
//     reapplying succeeds. Catches asymmetries between an *.up.sql
//     body and its corresponding *.down.sql body — e.g. a migration
//     that adds a column without dropping it on rollback, leaving
//     subsequent reapply in an inconsistent state.
//
// This file is build-tag-gated so the default `go test ./...` CI run
// does NOT spin Docker. Run locally with:
//
//	cd edge-control-plane
//	go test -tags=integration -v -count=1 ./migrations/...
//
// CI runs it under `go-test-integration` (services: postgres:16-alpine).
// See .github/workflows/ci.yml.
//
// Note on split files: the migrations in this directory are stored as
// `*.up.sql` + `*.down.sql` pairs (with `-- +migrate Up` / `-- +migrate
// Down` markers inline, courtesy of PR #259). rubenv's FileMigrationSource
// reads every .sql file, so each pair produces TWO Migration records:
// one with the Up populated and Down nil, one with Down populated and
// Up nil. The apply/rollback counts are therefore 2N where N is the
// number of logical migrations. That's fine — the test just asserts
// consistency of the two passes (apply N pairs → rollback N pairs →
// re-apply N pairs → gorp_migrations has the same count).

package migrations_test

import (
	"context"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"github.com/rubenv/sql-migrate"
	"github.com/stretchr/testify/require"
	tcpg "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/edgeclouderz/edge-cloud/edge-control-plane/internal/repository"
	"github.com/edgeclouderz/edge-cloud/edge-control-plane/internal/testutil"
)

// splitFileCount is the number of *.sql files in this directory.
// Each logical migration has one .up.sql and one .down.sql, so the
// apply + rollback paths will track this many records in gorp_migrations.
// Update when adding a new migration pair.
const splitFileCount = 40 // 20 .up.sql + 20 .down.sql on current main

// wantTables is the post-015 expected set of public-schema tables.
// Update when adding a migration that creates a new table. The
// roundtrip test asserts each is present after Up and absent after
// rolling back to v0.
var wantTables = []string{
	"tenants",
	"quotas",
	"api_keys",
	"deployments",
	"active_deployments",
	"app_env",
	"workers",
	"worker_status",
	"apps",
	"logs",
	"app_traffic_splits",
	"domains",
	"autoscale_events",
	"audit_logs",
	"webhooks",
	"webhook_deliveries",
}

// wantColumns enumerates the public-schema columns each table must
// have after the up migrations apply. Acts as a schema-vs-migration
// contract: if someone renames a column in a migration, drops one
// without updating this map, or splits a column across migrations and
// forgets one of the pieces, the UpAppliesAllAndCreatesTables subtest
// fails with the exact column that's missing.
//
// Update when:
//   - A migration adds a column to a tracked table → add the column
//     here, and ideally reference the migration number in the comment
//     so reviewers can trace the contract back to the schema change.
//   - A migration adds a new table → add a new entry.
//   - A migration renames or drops a column → rename/remove here in
//     the same PR so the test reflects the new shape.
//
// Note: this asserts column *existence*, not type, nullability, or
// constraint enforcement. The original migration files remain the
// source of truth for those properties — this test guards against
// accidental renames/drops, not against subtle type drift.
var wantColumns = map[string][]string{
	"tenants": {
		"id",
		"name",
		"plan",
		"allowlisted_destinations",
		"created_at",
	},
	"quotas": {
		"tenant_id",
		"max_deployments",        // 001
		"max_apps",               // 001
		"max_workers",            // 001
		"max_memory_mb",          // 001
		"max_outbound_mb",        // 001
		"used_outbound_bytes",    // 009_quotas_used_outbound
		"quota_period_start",     // 009_quotas_used_outbound
		"max_requests_per_month", // 013
		"used_request_count",     // 013
	},
	"api_keys": {
		"id",
		"tenant_id",
		"name",
		"key_hash",
		"role",
		"created_at",
		"last_used",
		"expires_at",
		"hash_algorithm", // 005_api_key_hash_algorithm
		"lookup_hash",    // 006_api_key_lookup_hash + 007 NOT NULL
	},
	"deployments": {
		"id",
		"tenant_id",
		"app_name",
		"status",
		"hash",
		"created_at",
		"regions",               // 008_deployments_regions
		"auto_rollback_enabled", // 009_add_auto_rollback
	},
	"active_deployments": {
		"tenant_id",
		"app_name",
		"deployment_id",
		"last_good_deployment_id", // 005_add_last_good
		"auto_rollback_enabled",   // 009_add_auto_rollback
		"stable_since",            // 009_add_auto_rollback
		"regions_published",       // 010_active_deployments_regions
		"regions_failed",          // 010_active_deployments_regions
		"last_publish_at",         // 010_active_deployments_regions
		"last_publish_attempt_id", // 010_active_deployments_regions
	},
	"app_env": {
		"tenant_id",
		"app_name",
		"env_key",
		"env_value",
	},
	"workers": {
		"id",
		"region",
		"ip",
		"memory_mb",
		"last_seen",
		"created_at",
		"tenant_id", // 003_workers_tenant_id
	},
	"worker_status": {
		"worker_id",
		"apps",
		"last_report",
	},
	"apps": {
		"id",
		"tenant_id",
		"name",
		"description",
		"created_at",
	},
	"logs": {
		"id",
		"tenant_id",
		"deployment_id",
		"app_name",
		"worker_id",
		"region",
		"level",
		"message",
		"labels",
		"ts",
	},
	"app_traffic_splits": {
		"tenant_id",
		"app_name",
		"deployment_id",
		"weight",
		"created_at",
	},
	"domains": {
		"id",
		"tenant_id",
		"app_name",
		"fqdn",
		"status",
		"last_error",
		"created_at",
		"verified_at",
	},
	"autoscale_events": {
		"id",
		"created_at",
		"region",
		"action",
		"from_count",
		"to_count",
		"reason",
		"provider_kind",
		"succeeded",
		"error_message",
	},
	"audit_logs": {
		"id",
		"tenant_id",
		"api_key_id",
		"role",
		"action",
		"resource",
		"resource_id",
		"details",
		"outcome",
		"error_msg",
		"request_ip",
		"created_at",
	},
	"webhooks": {
		"id",
		"tenant_id",
		"url",
		"secret",
		"events",
		"description",
		"enabled",
		"created_at",
	},
	"webhook_deliveries": {
		"id",
		"webhook_id",
		"event_type",
		"status",
		"status_code",
		"request_body",
		"response_body",
		"error_msg",
		"attempt",
		"max_attempts",
		"created_at",
		"completed_at",
	},
}

// TestRoundtrip is the headline acceptance test for the migration
// directory. Subtests share a single Postgres container + *sqlx.DB so
// the rollback and reapply steps build on the up pass. Failure in any
// subtest aborts siblings (default t.Run behaviour).
func TestRoundtrip(t *testing.T) {
	if reason, ok := testutil.ShouldSkipIntegration("SKIP_INTEGRATION_TESTS"); ok {
		t.Skip(reason)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pgC := newTestPostgres(t, ctx)
	t.Cleanup(func() {
		cctx, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		_ = pgC.Terminate(cctx)
	})

	db := newDBFromContainer(t, ctx, pgC)
	t.Cleanup(func() { _ = db.Close() })

	src := migrationsDir(t)

	t.Run("UpAppliesAllAndCreatesTables", func(t *testing.T) {
		n, err := migrate.Exec(db.DB, "postgres", &migrate.FileMigrationSource{Dir: src}, migrate.Up)
		require.NoError(t, err)
		require.Equal(t, splitFileCount, n)

		// rubenv tracks applied migrations in gorp_migrations (default
		// TableName; verified at migrate.go:50-55). Cross-check via
		// the tracking table instead of trusting the return value alone
		// — protects against future library changes to count semantics.
		var tracked int
		require.NoError(t, db.Get(&tracked, "SELECT COUNT(*) FROM gorp_migrations"))
		require.Equal(t, splitFileCount, tracked)

		for _, want := range wantTables {
			assertTableExists(t, db, want)
		}

		// Column-level contract: every expected column on every tracked
		// table must exist. Catches accidental renames/drops in a
		// migration that would otherwise leave the schema silently
		// drifting from what the Go repositories expect.
		for table, cols := range wantColumns {
			assertColumnsExist(t, db, table, cols)
		}
	})

	t.Run("DownReversesAllToVersionZero", func(t *testing.T) {
		// migrate.Exec(Down) walks every applied migration in reverse
		// and applies each Down section. ExecVersion(0, Down) would
		// fail because rubenv's planner looks up the target version
		// via VersionInt() (migrate.go:686) and no migration has
		// version-int 0 — the prefix regex starts at 1.
		n, err := migrate.Exec(db.DB, "postgres", &migrate.FileMigrationSource{Dir: src}, migrate.Down)
		require.NoError(t, err)
		require.Equal(t, splitFileCount, n)

		var tracked int
		require.NoError(t, db.Get(&tracked, "SELECT COUNT(*) FROM gorp_migrations"))
		require.Equal(t, 0, tracked)

		// Every public-schema table we created in the up pass should
		// now be gone. Using the same wantTables set catches migrations
		// whose Down section silently leaks a table.
		for _, want := range wantTables {
			assertTableAbsent(t, db, want)
		}
	})

	t.Run("UpReappliesCleanlyFromEmpty", func(t *testing.T) {
		n, err := migrate.Exec(db.DB, "postgres", &migrate.FileMigrationSource{Dir: src}, migrate.Up)
		require.NoError(t, err)
		require.Equal(t, splitFileCount, n)
	})

	t.Run("MigrationsAreLexicographicallyOrdered", func(t *testing.T) {
		// rubenv applies migrations in byId order, which is a
		// lexicographic sort on the file name. The numeric prefix
		// (NNN_) must be zero-padded (or otherwise lexically sortable)
		// so e.g. 002_*.sql sorts before 010_*.sql. This catches a
		// common foot-gun where someone adds a migration named
		// `2_*.sql` instead of `002_*.sql` and the apply order
		// silently breaks.
		assertMigrationsLexicallyOrdered(t, src)
	})
}

// newTestPostgres boots a postgres:16-alpine testcontainer. We use
// BasicWaitStrategies so the container reports "ready" only after
// pg_isready succeeds — without it the first connection from
// repository.NewDB can race the inner pg_isready loop on Mac/Windows
// runners and flake.
func newTestPostgres(t *testing.T, ctx context.Context) *tcpg.PostgresContainer {
	t.Helper()
	pgC, err := tcpg.Run(ctx,
		"postgres:16-alpine",
		tcpg.WithDatabase("edgecloud_test"),
		tcpg.WithUsername("test"),
		tcpg.WithPassword("test"),
		tcpg.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	require.NotNil(t, pgC)
	return pgC
}

// newDBFromContainer opens a *sqlx.DB via the production NewDB helper
// (internal/repository/db.go:27). Reusing the helper means the test
// exercises the same MaxOpenConns/MaxIdleConns/ConnMaxLifetime config
// as the API server, not a side-channel configuration.
func newDBFromContainer(t *testing.T, ctx context.Context, pgC *tcpg.PostgresContainer) *sqlx.DB {
	t.Helper()
	// testcontainers' ConnectionString returns `postgres://...?` with no
	// query params, which lib/pq parses as sslmode=require — Postgres
	// 16-alpine defaults to SSL enabled, so the connection fails with
	// "pq: SSL is not enabled on the server". Passing sslmode=disable
	// explicitly matches the production DSN format in
	// internal/config/config.go:DatabaseConfig.DSN().
	connStr, err := pgC.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	db, err := repository.NewDB(connStr)
	require.NoError(t, err)
	return db
}

// migrationsDir resolves the migrations directory from this file's
// own location via runtime.Caller(0). Avoids depending on the runner's
// working directory, so `go test ./migrations/...` from any CWD lands
// in the same place.
func migrationsDir(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	return filepath.Dir(here)
}

func assertTableExists(t *testing.T, db *sqlx.DB, name string) {
	t.Helper()
	var n int
	require.NoError(t, db.Get(&n,
		"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='public' AND table_name=$1",
		name))
	require.Equalf(t, 1, n, "table %q missing after migrations", name)
}

func assertTableAbsent(t *testing.T, db *sqlx.DB, name string) {
	t.Helper()
	var n int
	require.NoError(t, db.Get(&n,
		"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='public' AND table_name=$1",
		name))
	require.Equalf(t, 0, n, "table %q still present after rollback to v0", name)
}

func assertColumnsExist(t *testing.T, db *sqlx.DB, table string, columns []string) {
	t.Helper()
	for _, col := range columns {
		var n int
		require.NoError(t, db.Get(&n,
			"SELECT COUNT(*) FROM information_schema.columns WHERE table_schema='public' AND table_name=$1 AND column_name=$2",
			table, col))
		require.Equalf(t, 1, n, "table %q is missing expected column %q after migrations", table, col)
	}
}

// assertMigrationsLexicallyOrdered guards against two classes of bug:
//
//  1. Non-zero-padded prefix: a new migration named `2_*.sql` instead
//     of `002_*.sql` would sort AFTER `10_*.sql`, breaking the apply
//     order silently. Catches it at PR time.
//
//  2. Missing pair: each `NNN_name.up.sql` must have a matching
//     `NNN_name.down.sql` (rubenv produces one Migration record per
//     file; an orphan up or down file would be tracked but never
//     produce SQL on one side of the round-trip).
//
// Note on order: with the split-file format, lexicographic order
// interleaves as `001.down.sql, 001.up.sql, 002.down.sql, 002.up.sql,
// ...` because 'd' < 'u'. That's fine — each side of the pair has
// the opposite direction's SQL as empty, so the net effect applies
// migrations in logical order.
func assertMigrationsLexicallyOrdered(t *testing.T, dir string) {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	require.NoError(t, err)

	// Map each base name to the set of directions present.
	pairs := make(map[string]map[string]struct{})
	for _, e := range entries {
		name := filepath.Base(e)
		switch {
		case strings.HasSuffix(name, ".up.sql"):
			base := strings.TrimSuffix(name, ".up.sql")
			if pairs[base] == nil {
				pairs[base] = map[string]struct{}{}
			}
			pairs[base]["up"] = struct{}{}
		case strings.HasSuffix(name, ".down.sql"):
			base := strings.TrimSuffix(name, ".down.sql")
			if pairs[base] == nil {
				pairs[base] = map[string]struct{}{}
			}
			pairs[base]["down"] = struct{}{}
		default:
			t.Fatalf("migration file %q does not match *.up.sql or *.down.sql", name)
		}
	}

	// Every base must have both up and down files.
	for base, dirs := range pairs {
		_, hasUp := dirs["up"]
		_, hasDown := dirs["down"]
		require.Truef(t, hasUp, "missing .up.sql for %s", base)
		require.Truef(t, hasDown, "missing .down.sql for %s", base)
	}

	// Sort by the numeric prefix to detect `2_*.sql` < `10_*.sql` mistakes.
	bases := make([]string, 0, len(pairs))
	for base := range pairs {
		bases = append(bases, base)
	}
	sort.Strings(bases)
	// The first underscore-separated token must be a zero-padded
	// integer, AND the lex order must be non-decreasing in the parsed
	// integer. Multiple migrations can share a prefix (e.g. 005_add_last_good,
	// 005_api_key_hash_algorithm, 005_logs); equal prefixes are fine,
	// but a smaller int after a larger one means the prefix wasn't
	// zero-padded (e.g. "10_*" sort after "2_*" instead of before).
	prev := -1
	for _, base := range bases {
		idx := strings.Index(base, "_")
		require.Greaterf(t, idx, 0, "migration %q has no NNN_ prefix", base)
		num, err := strconv.Atoi(base[:idx])
		require.NoErrorf(t, err, "migration %q prefix is not a pure integer; zero-pad it (e.g. 002_)", base)
		require.GreaterOrEqualf(t, num, prev,
			"migration %q (numeric prefix %d) sorts before %d — looks like a non-zero-padded prefix",
			base, num, prev)
		prev = num
	}
}
