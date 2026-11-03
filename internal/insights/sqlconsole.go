package insights

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// The SQL console runs one read-only query at a time on a separate connection opened with
// mode=ro and query_only, so a statement cannot change the database even if it slips past the
// statement check. Only the management key may use it.

const (
	sqlMaxQueryBytes = 20000
	sqlMaxRows       = 1000
	sqlMaxCellBytes  = 2000
	sqlQueryTimeout  = 20 * time.Second
)

// SQLResult is the output of one console query.
type SQLResult struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Truncated bool     `json:"truncated"`
	ElapsedMs int64    `json:"elapsed_ms"`
}

// TableSchema lists one table's columns for the console's reference panel.
type TableSchema struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Rows    int64    `json:"rows"`
}

// checkReadOnlySQL accepts a single SELECT, WITH or VALUES statement and returns it without trailing
// semicolons.
func checkReadOnlySQL(query string) (string, error) {
	query = strings.TrimSpace(query)
	if len(query) > sqlMaxQueryBytes {
		return "", fmt.Errorf("query is longer than %d bytes", sqlMaxQueryBytes)
	}
	for strings.HasSuffix(query, ";") {
		query = strings.TrimSpace(strings.TrimSuffix(query, ";"))
	}
	if query == "" {
		return "", fmt.Errorf("enter a query")
	}
	firstWord := ""
	for i := 0; i < len(query); {
		c := query[i]
		switch {
		case c == '\'' || c == '"' || c == '`' || c == '[':
			closing := c
			if c == '[' {
				closing = ']'
			}
			end := strings.IndexByte(query[i+1:], closing)
			if end < 0 {
				return "", fmt.Errorf("unterminated quote")
			}
			i += end + 2
		case c == '-' && i+1 < len(query) && query[i+1] == '-':
			end := strings.IndexByte(query[i:], '\n')
			if end < 0 {
				i = len(query)
			} else {
				i += end + 1
			}
		case c == '/' && i+1 < len(query) && query[i+1] == '*':
			end := strings.Index(query[i+2:], "*/")
			if end < 0 {
				return "", fmt.Errorf("unterminated comment")
			}
			i += end + 4
		case c == ';':
			return "", fmt.Errorf("run one statement at a time")
		case firstWord == "" && isWordByte(c):
			start := i
			for i < len(query) && isWordByte(query[i]) {
				i++
			}
			firstWord = strings.ToUpper(query[start:i])
		default:
			i++
		}
	}
	switch firstWord {
	case "SELECT", "WITH", "VALUES":
		return query, nil
	}
	return "", fmt.Errorf("only SELECT, WITH and VALUES queries are allowed")
}

func isWordByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// readOnlyDB opens the console's read-only connection on first use.
func (s *Store) readOnlyDB() (*sql.DB, error) {
	s.readOnlyMu.Lock()
	defer s.readOnlyMu.Unlock()
	if s.readOnly != nil {
		return s.readOnly, nil
	}
	dsn := "file:" + filepath.ToSlash(s.path) + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	db, errOpen := sql.Open("sqlite", dsn)
	if errOpen != nil {
		return nil, fmt.Errorf("insights: open read-only database: %w", errOpen)
	}
	db.SetMaxOpenConns(2)
	s.readOnly = db
	return db, nil
}

// runSQL executes one read-only query and returns at most sqlMaxRows rows.
func (s *Store) runSQL(ctx context.Context, query string) (*SQLResult, error) {
	checked, errCheck := checkReadOnlySQL(query)
	if errCheck != nil {
		return nil, errCheck
	}
	db, errOpen := s.readOnlyDB()
	if errOpen != nil {
		return nil, errOpen
	}
	ctx, cancel := context.WithTimeout(ctx, sqlQueryTimeout)
	defer cancel()
	started := time.Now()
	rows, errQuery := db.QueryContext(ctx, checked)
	if errQuery != nil {
		return nil, errQuery
	}
	columns, errColumns := rows.Columns()
	if errColumns != nil {
		_ = rows.Close()
		return nil, errColumns
	}
	out := &SQLResult{Columns: columns, Rows: [][]any{}}
	for rows.Next() {
		if len(out.Rows) == sqlMaxRows {
			out.Truncated = true
			break
		}
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if errScan := rows.Scan(targets...); errScan != nil {
			_ = rows.Close()
			return nil, errScan
		}
		for i, value := range values {
			values[i] = consoleValue(value)
		}
		out.Rows = append(out.Rows, values)
	}
	if errClose := closeRows(rows); errClose != nil {
		return nil, errClose
	}
	out.ElapsedMs = time.Since(started).Milliseconds()
	return out, nil
}

// consoleValue converts a scanned value to something JSON shows faithfully, capping long text.
func consoleValue(value any) any {
	switch v := value.(type) {
	case []byte:
		if utf8.Valid(v) {
			return truncateText(string(v), sqlMaxCellBytes)
		}
		if len(v) > sqlMaxCellBytes/2 {
			v = v[:sqlMaxCellBytes/2]
		}
		return "x'" + hex.EncodeToString(v) + "'"
	case string:
		return truncateText(v, sqlMaxCellBytes)
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano)
	}
	return value
}

// schemaInfo lists the database tables and their columns.
func (s *Store) schemaInfo(ctx context.Context) ([]TableSchema, error) {
	db, errOpen := s.readOnlyDB()
	if errOpen != nil {
		return nil, errOpen
	}
	rows, errQuery := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if errQuery != nil {
		return nil, fmt.Errorf("insights: list tables: %w", errQuery)
	}
	var out []TableSchema
	for rows.Next() {
		var table TableSchema
		if errScan := rows.Scan(&table.Name); errScan != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("insights: scan table: %w", errScan)
		}
		out = append(out, table)
	}
	if errClose := closeRows(rows); errClose != nil {
		return nil, errClose
	}
	for i := range out {
		columns, errColumns := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, out[i].Name)
		if errColumns != nil {
			return nil, fmt.Errorf("insights: list columns: %w", errColumns)
		}
		for columns.Next() {
			var name string
			if errScan := columns.Scan(&name); errScan != nil {
				_ = columns.Close()
				return nil, fmt.Errorf("insights: scan column: %w", errScan)
			}
			out[i].Columns = append(out[i].Columns, name)
		}
		if errClose := closeRows(columns); errClose != nil {
			return nil, errClose
		}
		// Table names come from sqlite_master, so quoting them is safe.
		quoted := `"` + strings.ReplaceAll(out[i].Name, `"`, `""`) + `"`
		if errCount := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+quoted).Scan(&out[i].Rows); errCount != nil {
			return nil, fmt.Errorf("insights: count %s: %w", out[i].Name, errCount)
		}
	}
	return out, nil
}
