package insights

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// schema is applied idempotently on open. Timestamps are Unix milliseconds (UTC).
const schema = `
CREATE TABLE IF NOT EXISTS requests (
	id                 INTEGER PRIMARY KEY,
	ts_ms              INTEGER NOT NULL,
	request_id         TEXT,
	trace_id           TEXT,
	provider           TEXT NOT NULL,
	model              TEXT,
	alias              TEXT,
	response_model     TEXT,
	auth_id            TEXT,
	auth_index         TEXT,
	auth_type          TEXT,
	account            TEXT,
	api_key_fp         TEXT,
	api_key_label      TEXT,
	client_ip          TEXT,
	user_agent         TEXT,
	client_kind        TEXT,
	session_id         TEXT,
	parent_session_id  TEXT,
	stream             INTEGER NOT NULL DEFAULT 0,
	failed             INTEGER NOT NULL DEFAULT 0,
	status_code        INTEGER,
	fail_code          TEXT,
	latency_ms         INTEGER,
	ttft_ms            INTEGER,
	input_tokens       INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
	cache_write_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens      INTEGER NOT NULL DEFAULT 0,
	reasoning_tokens   INTEGER NOT NULL DEFAULT 0,
	total_tokens       INTEGER NOT NULL DEFAULT 0,
	reasoning_effort   TEXT,
	service_tier       TEXT,
	error_class        TEXT,
	cost_usd           REAL
);
CREATE INDEX IF NOT EXISTS idx_requests_ts ON requests(ts_ms);
CREATE INDEX IF NOT EXISTS idx_requests_session ON requests(session_id, ts_ms);
CREATE INDEX IF NOT EXISTS idx_requests_auth_ts ON requests(auth_id, ts_ms);

CREATE TABLE IF NOT EXISTS quota_samples (
	id             INTEGER PRIMARY KEY,
	ts_ms          INTEGER NOT NULL,
	auth_id        TEXT NOT NULL,
	provider       TEXT NOT NULL,
	window_key     TEXT NOT NULL,
	used_pct       REAL NOT NULL,
	resets_at_ms   INTEGER NOT NULL DEFAULT 0,
	window_minutes INTEGER NOT NULL DEFAULT 0,
	status         TEXT
);
CREATE INDEX IF NOT EXISTS idx_quota_auth_window_ts ON quota_samples(auth_id, window_key, ts_ms);
CREATE INDEX IF NOT EXISTS idx_quota_ts ON quota_samples(ts_ms);

CREATE TABLE IF NOT EXISTS accounts (
	auth_id       TEXT PRIMARY KEY,
	provider      TEXT NOT NULL,
	label         TEXT,
	email         TEXT,
	plan          TEXT,
	first_seen_ms INTEGER NOT NULL,
	last_seen_ms  INTEGER NOT NULL,
	monthly_usd   REAL
);

CREATE TABLE IF NOT EXISTS alerts (
	id             INTEGER PRIMARY KEY,
	ts_ms          INTEGER NOT NULL,
	dedupe_key     TEXT NOT NULL UNIQUE,
	kind           TEXT NOT NULL,
	severity       TEXT NOT NULL,
	provider       TEXT,
	auth_id        TEXT,
	window_key     TEXT,
	title          TEXT NOT NULL,
	body           TEXT NOT NULL,
	push_status    TEXT NOT NULL DEFAULT '',
	push_error     TEXT NOT NULL DEFAULT '',
	push_attempts  INTEGER NOT NULL DEFAULT 0,
	acked_ms       INTEGER,
	fingerprint    TEXT,
	resolves_ms    INTEGER,
	resolved_ms    INTEGER,
	resolve_status TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_alerts_ts ON alerts(ts_ms);

CREATE TABLE IF NOT EXISTS access_failures (
	id          INTEGER PRIMARY KEY,
	ts_ms       INTEGER NOT NULL,
	kind        TEXT NOT NULL,
	status_code INTEGER NOT NULL,
	client_ip   TEXT,
	route       TEXT
);
CREATE INDEX IF NOT EXISTS idx_access_failures_ts ON access_failures(ts_ms);

CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// schemaVersion is stored in PRAGMA user_version. Version 1 adds error classes, API-equivalent cost
// and plan prices to databases created before them; version 2 adds Luna delivery and resolution to alerts.
const schemaVersion = 2

// Store wraps the insights SQLite database.
type Store struct {
	db   *sql.DB
	path string
	// readOnly is a separate read-only handle for the SQL console, opened on first use.
	readOnlyMu sync.Mutex
	readOnly   *sql.DB
}

// requestRow is one persisted usage record. It never contains prompt or response content.
type requestRow struct {
	TS               time.Time
	RequestID        string
	TraceID          string
	Provider         string
	Model            string
	Alias            string
	ResponseModel    string
	AuthID           string
	AuthIndex        string
	AuthType         string
	Account          string
	APIKeyFP         string
	APIKeyLabel      string
	ClientIP         string
	UserAgent        string
	ClientKind       string
	SessionID        string
	ParentSessionID  string
	Stream           bool
	Failed           bool
	StatusCode       int
	FailCode         string
	LatencyMs        int64
	TTFTMs           int64
	InputTokens      int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	OutputTokens     int64
	ReasoningTokens  int64
	TotalTokens      int64
	ReasoningEffort  string
	ServiceTier      string
	// ErrorClass names why a failed request failed (see classifyFailure); empty on success.
	ErrorClass string
	// CostUSD is the API list-price equivalent; invalid when the model has no known price.
	CostUSD sql.NullFloat64
}

// quotaRow is one persisted quota window observation.
type quotaRow struct {
	TS            time.Time
	AuthID        string
	Provider      string
	WindowKey     string
	UsedPct       float64
	ResetsAt      time.Time
	WindowMinutes int64
	Status        string
}

// accountRow is the latest known identity of a credential.
type accountRow struct {
	AuthID   string
	Provider string
	Label    string
	Email    string
	Plan     string
	SeenAt   time.Time
}

// OpenStore opens (and creates when missing) the insights database at path.
func OpenStore(path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("insights: empty database path")
	}
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
		return nil, fmt.Errorf("insights: create database directory: %w", errMkdir)
	}
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, errOpen := sql.Open("sqlite", dsn)
	if errOpen != nil {
		return nil, fmt.Errorf("insights: open database: %w", errOpen)
	}
	db.SetMaxOpenConns(4)
	store := &Store{db: db, path: path}
	errSchema := func() error {
		if _, errExec := db.Exec(schema); errExec != nil {
			return fmt.Errorf("insights: apply schema: %w", errExec)
		}
		return store.migrate(context.Background())
	}()
	if errSchema != nil {
		if errClose := db.Close(); errClose != nil {
			return nil, fmt.Errorf("%w (close: %v)", errSchema, errClose)
		}
		return nil, errSchema
	}
	return store, nil
}

// migrate upgrades databases created by earlier versions. Each step is idempotent.
func (s *Store) migrate(ctx context.Context) error {
	var version int
	if errScan := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); errScan != nil {
		return fmt.Errorf("insights: read schema version: %w", errScan)
	}
	if version >= schemaVersion {
		return nil
	}
	for _, column := range []struct{ table, name, kind string }{
		{"requests", "error_class", "TEXT"},
		{"requests", "cost_usd", "REAL"},
		{"accounts", "monthly_usd", "REAL"},
		{"alerts", "fingerprint", "TEXT"},
		{"alerts", "resolves_ms", "INTEGER"},
		{"alerts", "resolved_ms", "INTEGER"},
		{"alerts", "resolve_status", "TEXT NOT NULL DEFAULT ''"},
	} {
		if errAdd := s.addColumnIfMissing(ctx, column.table, column.name, column.kind); errAdd != nil {
			return errAdd
		}
	}
	if version < 1 {
		if errBackfill := s.backfillErrorClasses(ctx); errBackfill != nil {
			return errBackfill
		}
	}
	if _, errExec := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_alerts_fingerprint ON alerts(fingerprint)`); errExec != nil {
		return fmt.Errorf("insights: index alert fingerprints: %w", errExec)
	}
	// Alerts from before version 2 were never sent to Luna; their fingerprint is their dedupe key.
	if _, errExec := s.db.ExecContext(ctx, `UPDATE alerts SET fingerprint = dedupe_key WHERE fingerprint IS NULL`); errExec != nil {
		return fmt.Errorf("insights: backfill alert fingerprints: %w", errExec)
	}
	if _, errExec := s.db.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); errExec != nil {
		return fmt.Errorf("insights: write schema version: %w", errExec)
	}
	return nil
}

// addColumnIfMissing adds a column; table, name and kind are trusted constants.
func (s *Store) addColumnIfMissing(ctx context.Context, table, name, kind string) error {
	var count int
	if errScan := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, name).Scan(&count); errScan != nil {
		return fmt.Errorf("insights: inspect %s.%s: %w", table, name, errScan)
	}
	if count > 0 {
		return nil
	}
	if _, errExec := s.db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+name+" "+kind); errExec != nil {
		return fmt.Errorf("insights: add %s.%s: %w", table, name, errExec)
	}
	return nil
}

// backfillErrorClasses classifies failures recorded before error bodies were classified. Without
// the body, a Claude 429 that the same session retried successfully within 15 seconds is taken to
// be a fast-mode refusal: Claude Code retries those at standard speed at once, while a real rate
// limit keeps failing.
func (s *Store) backfillErrorClasses(ctx context.Context) error {
	_, errExec := s.db.ExecContext(ctx, `UPDATE requests SET error_class = CASE
			WHEN status_code = 429 AND provider = 'claude' AND session_id <> '' AND EXISTS (
				SELECT 1 FROM requests b WHERE b.session_id = requests.session_id AND b.failed = 0
					AND b.ts_ms >= requests.ts_ms AND b.ts_ms <= requests.ts_ms + 15000) THEN 'fast_mode'
			WHEN status_code = 429 THEN 'rate_limit'
			WHEN status_code IN (401, 403) THEN 'upstream_auth'
			WHEN status_code = 499 THEN 'client_cancelled'
			WHEN status_code = 529 THEN 'overloaded'
			WHEN status_code = 404 THEN 'model_not_found'
			WHEN status_code >= 500 THEN 'upstream_error'
			WHEN status_code >= 400 THEN 'bad_request'
			ELSE 'other' END
		WHERE failed = 1 AND error_class IS NULL`)
	if errExec != nil {
		return fmt.Errorf("insights: backfill error classes: %w", errExec)
	}
	return nil
}

// metaValue reads a settings value; missing keys return "".
func (s *Store) metaValue(ctx context.Context, key string) (string, error) {
	var value string
	errScan := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if errScan == sql.ErrNoRows {
		return "", nil
	}
	if errScan != nil {
		return "", fmt.Errorf("insights: read %s: %w", key, errScan)
	}
	return value, nil
}

func (s *Store) setMetaValue(ctx context.Context, key, value string) error {
	if _, errExec := s.db.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); errExec != nil {
		return fmt.Errorf("insights: write %s: %w", key, errExec)
	}
	return nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	s.readOnlyMu.Lock()
	if s.readOnly != nil {
		if errClose := s.readOnly.Close(); errClose != nil {
			s.readOnlyMu.Unlock()
			_ = s.db.Close()
			return fmt.Errorf("insights: close read-only handle: %w", errClose)
		}
		s.readOnly = nil
	}
	s.readOnlyMu.Unlock()
	return s.db.Close()
}

// Path returns the database file path.
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

func (s *Store) insertRequests(ctx context.Context, rows []requestRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, errBegin := s.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return fmt.Errorf("insights: begin request batch: %w", errBegin)
	}
	stmt, errPrepare := tx.PrepareContext(ctx, `INSERT INTO requests (
		ts_ms, request_id, trace_id, provider, model, alias, response_model, auth_id, auth_index, auth_type,
		account, api_key_fp, api_key_label, client_ip, user_agent, client_kind, session_id, parent_session_id,
		stream, failed, status_code, fail_code, latency_ms, ttft_ms, input_tokens, cache_read_tokens,
		cache_write_tokens, output_tokens, reasoning_tokens, total_tokens, reasoning_effort, service_tier,
		error_class, cost_usd
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if errPrepare != nil {
		_ = tx.Rollback()
		return fmt.Errorf("insights: prepare request insert: %w", errPrepare)
	}
	for _, row := range rows {
		if _, errExec := stmt.ExecContext(ctx,
			row.TS.UnixMilli(), row.RequestID, row.TraceID, row.Provider, row.Model, row.Alias, row.ResponseModel,
			row.AuthID, row.AuthIndex, row.AuthType, row.Account, row.APIKeyFP, row.APIKeyLabel, row.ClientIP,
			row.UserAgent, row.ClientKind, row.SessionID, row.ParentSessionID, boolInt(row.Stream), boolInt(row.Failed),
			row.StatusCode, row.FailCode, row.LatencyMs, row.TTFTMs, row.InputTokens, row.CacheReadTokens,
			row.CacheWriteTokens, row.OutputTokens, row.ReasoningTokens, row.TotalTokens, row.ReasoningEffort,
			row.ServiceTier, nullString(row.ErrorClass), row.CostUSD,
		); errExec != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return fmt.Errorf("insights: insert request: %w", errExec)
		}
	}
	if errClose := stmt.Close(); errClose != nil {
		_ = tx.Rollback()
		return fmt.Errorf("insights: close request statement: %w", errClose)
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return fmt.Errorf("insights: commit request batch: %w", errCommit)
	}
	return nil
}

func (s *Store) insertAccessFailures(ctx context.Context, rows []accessFailureRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, errBegin := s.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return fmt.Errorf("insights: begin access failure batch: %w", errBegin)
	}
	for _, row := range rows {
		if _, errExec := tx.ExecContext(ctx, `INSERT INTO access_failures (ts_ms, kind, status_code, client_ip, route) VALUES (?,?,?,?,?)`,
			row.TS.UnixMilli(), row.Kind, row.StatusCode, row.ClientIP, row.Route); errExec != nil {
			_ = tx.Rollback()
			return fmt.Errorf("insights: insert access failure: %w", errExec)
		}
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return fmt.Errorf("insights: commit access failures: %w", errCommit)
	}
	return nil
}

func (s *Store) insertQuotaSamples(ctx context.Context, rows []quotaRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, errBegin := s.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return fmt.Errorf("insights: begin quota batch: %w", errBegin)
	}
	for _, row := range rows {
		resetsAt := int64(0)
		if !row.ResetsAt.IsZero() {
			resetsAt = row.ResetsAt.UnixMilli()
		}
		if _, errExec := tx.ExecContext(ctx, `INSERT INTO quota_samples
			(ts_ms, auth_id, provider, window_key, used_pct, resets_at_ms, window_minutes, status)
			VALUES (?,?,?,?,?,?,?,?)`,
			row.TS.UnixMilli(), row.AuthID, row.Provider, row.WindowKey, row.UsedPct, resetsAt, row.WindowMinutes, row.Status,
		); errExec != nil {
			_ = tx.Rollback()
			return fmt.Errorf("insights: insert quota sample: %w", errExec)
		}
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return fmt.Errorf("insights: commit quota batch: %w", errCommit)
	}
	return nil
}

func (s *Store) upsertAccounts(ctx context.Context, rows []accountRow) error {
	for _, row := range rows {
		seen := row.SeenAt.UnixMilli()
		if _, errExec := s.db.ExecContext(ctx, `INSERT INTO accounts
			(auth_id, provider, label, email, plan, first_seen_ms, last_seen_ms) VALUES (?,?,?,?,?,?,?)
			ON CONFLICT(auth_id) DO UPDATE SET
				provider = excluded.provider,
				label = excluded.label,
				email = CASE WHEN excluded.email <> '' THEN excluded.email ELSE accounts.email END,
				plan = CASE WHEN excluded.plan <> '' THEN excluded.plan ELSE accounts.plan END,
				last_seen_ms = MAX(accounts.last_seen_ms, excluded.last_seen_ms)`,
			row.AuthID, row.Provider, row.Label, row.Email, row.Plan, seen, seen,
		); errExec != nil {
			return fmt.Errorf("insights: upsert account: %w", errExec)
		}
	}
	return nil
}

// pruneBefore removes rows older than cutoff. It is only used when a retention period is configured.
func (s *Store) pruneBefore(ctx context.Context, cutoff time.Time) error {
	cutoffMs := cutoff.UnixMilli()
	if _, errExec := s.db.ExecContext(ctx, `DELETE FROM requests WHERE ts_ms < ?`, cutoffMs); errExec != nil {
		return fmt.Errorf("insights: prune requests: %w", errExec)
	}
	if _, errExec := s.db.ExecContext(ctx, `DELETE FROM quota_samples WHERE ts_ms < ?`, cutoffMs); errExec != nil {
		return fmt.Errorf("insights: prune quota samples: %w", errExec)
	}
	if _, errExec := s.db.ExecContext(ctx, `DELETE FROM alerts WHERE ts_ms < ?`, cutoffMs); errExec != nil {
		return fmt.Errorf("insights: prune alerts: %w", errExec)
	}
	if _, errExec := s.db.ExecContext(ctx, `DELETE FROM access_failures WHERE ts_ms < ?`, cutoffMs); errExec != nil {
		return fmt.Errorf("insights: prune access failures: %w", errExec)
	}
	return nil
}

func nullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
