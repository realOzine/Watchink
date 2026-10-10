// Package store owns the SQLite database and the files of a data directory.
// It is the only package that imports the SQLite
// driver.
//
// # Transactions
//
// Every repository method is defined on *Queries, which is embedded in both
// *Store and *Tx:
//
//   - called on a *Store, a method runs on its own: reads use the read pool,
//     writes use the single writer connection, and multi-statement writes are
//     wrapped in their own transaction;
//   - called on a *Tx (inside Store.Write), a method takes part in the
//     caller's transaction, so several tables can be changed atomically.
//
// All writes are serialized through one connection. Consequently a
// Store.Write callback must only use the *Tx it is given: calling a writing
// method on the *Store from inside the callback would wait for the connection
// the callback itself is holding (until its context expires).
//
// # Conventions
//
// Nullable TEXT columns map to Go strings with "" meaning NULL. Instants are
// RFC 3339 UTC strings (app.FormatTime). Get* methods return a NOT_FOUND
// *app.Error when the row is missing; Find*, Latest*, Active* and Current*
// methods return (nil, nil). Paths stored in the database are relative to the
// data directory and use forward slashes.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"modernc.org/sqlite"

	"watchink/internal/app"
)

// DBFileName is the database file name inside the data directory.
const DBFileName = "watchink.db"

// ErrSchemaTooNew is the cause of the error returned by Open when the
// database was written by a newer program version.
var ErrSchemaTooNew = errors.New("store: database schema is newer than this program supports")

//go:embed migrations/*.sql
var migrationFS embed.FS

// migration is one schema step; applying it moves user_version to version.
type migration struct {
	version int
	name    string
	sql     string
}

// Store is an open data directory: the database plus the file helpers.
type Store struct {
	// Queries runs each repository call on its own (auto-transaction).
	*Queries

	dataDir string
	w       *sql.DB // single writer connection
	r       *sql.DB // read pool
}

// Tx is a write transaction started by Store.Write.
type Tx struct {
	// Queries runs repository calls inside this transaction.
	*Queries
}

// Queries carries the repository methods. Obtain one from Store.Queries or
// Tx.Queries; helpers that should work both inside and outside a transaction
// can accept a *Queries.
type Queries struct {
	s  *Store
	tx *sql.Tx // nil: auto mode
}

// Open opens (creating and migrating when needed) the database of dataDir.
func Open(ctx context.Context, dataDir string) (*Store, error) {
	migs, err := embeddedMigrations()
	if err != nil {
		return nil, internalErr("读取内置迁移失败", err)
	}
	return openWith(ctx, dataDir, migs)
}

// SchemaVersion is the schema version this program supports.
func SchemaVersion() int {
	migs, err := embeddedMigrations()
	if err != nil || len(migs) == 0 {
		return 0
	}
	return migs[len(migs)-1].version
}

func embeddedMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var migs []migration
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		num, _, _ := strings.Cut(name, "_")
		v, err := strconv.Atoi(num)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("migration %q: name must start with a positive number", name)
		}
		b, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		migs = append(migs, migration{version: v, name: name, sql: string(b)})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })
	for i, m := range migs {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration %q: versions must be contiguous from 1", m.name)
		}
	}
	return migs, nil
}

// dsn builds a modernc DSN. The path is percent-encoded because the default
// data directory contains spaces ("Application Support").
func dsn(path string, readOnly bool) string {
	q := []string{
		"_pragma=busy_timeout(5000)",
		"_pragma=journal_mode(WAL)",
		"_pragma=synchronous(NORMAL)",
		"_pragma=foreign_keys(ON)",
	}
	if readOnly {
		q = append(q, "_pragma=query_only(1)")
	} else {
		// The writer takes the write lock at BEGIN so a transaction never has
		// to upgrade a read lock halfway through.
		q = append(q, "_txlock=immediate")
	}
	u := url.URL{Path: filepath.ToSlash(path)}
	return "file:" + u.EscapedPath() + "?" + strings.Join(q, "&")
}

func openWith(ctx context.Context, dataDir string, migs []migration) (_ *Store, err error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, internalErr("数据目录路径无效", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fileErr("无法创建数据目录", err)
	}
	dbPath := filepath.Join(abs, DBFileName)

	w, err := sql.Open("sqlite", dsn(dbPath, false))
	if err != nil {
		return nil, internalErr("无法打开数据库", err)
	}
	// every write goes through this one connection.
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)
	defer func() {
		if err != nil {
			w.Close()
		}
	}()
	if err := w.PingContext(ctx); err != nil {
		return nil, internalErr("无法打开数据库", err)
	}
	if err := migrate(ctx, w, dbPath, migs); err != nil {
		return nil, err
	}

	r, err := sql.Open("sqlite", dsn(dbPath, true))
	if err != nil {
		return nil, internalErr("无法打开数据库", err)
	}
	n := min(max(runtime.NumCPU(), 2), 8)
	r.SetMaxOpenConns(n)
	r.SetMaxIdleConns(n)
	if err := r.PingContext(ctx); err != nil {
		r.Close()
		return nil, internalErr("无法打开数据库", err)
	}

	s := &Store{dataDir: abs, w: w, r: r}
	s.Queries = &Queries{s: s}
	return s, nil
}

func migrate(ctx context.Context, w *sql.DB, dbPath string, migs []migration) error {
	var cur int
	if err := w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&cur); err != nil {
		return internalErr("无法读取数据库版本", err)
	}
	target := 0
	if len(migs) > 0 {
		target = migs[len(migs)-1].version
	}
	if cur > target {
		// an old program must never touch newer data.
		e := app.E(app.CodeInternal, "数据库版本（%d）高于本程序支持的版本（%d），已拒绝打开", cur, target).
			WithHint("请升级 Watchink 到最新版本后再使用该数据目录").
			WithDetail("db_version", cur).WithDetail("supported_version", target).
			WithCause(ErrSchemaTooNew)
		e.Retryable = false
		e.UserActionRequired = true
		return e
	}
	if cur == target {
		return nil
	}
	if cur > 0 {
		// copy the existing database aside first. VACUUM INTO produces
		// a consistent single-file copy that includes the WAL content.
		bak := fmt.Sprintf("%s.bak-%d", dbPath, cur)
		if err := os.Remove(bak); err != nil && !os.IsNotExist(err) {
			return fileErr("无法覆盖旧的数据库备份", err)
		}
		if _, err := w.ExecContext(ctx, "VACUUM INTO ?", bak); err != nil {
			return internalErr("迁移前备份数据库失败", err)
		}
	}
	for _, m := range migs {
		if m.version <= cur {
			continue
		}
		tx, err := w.BeginTx(ctx, nil)
		if err != nil {
			return internalErr("数据库迁移失败", err)
		}
		if _, err = tx.ExecContext(ctx, m.sql); err == nil {
			// user_version is part of the database header and therefore
			// transactional: schema and version move together.
			_, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version))
		}
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
		if err != nil {
			return internalErr(fmt.Sprintf("数据库迁移 %s 失败", m.name), err)
		}
	}
	return nil
}

// Close closes the database. The WAL is checkpointed by SQLite when the last
// connection closes.
func (s *Store) Close() error {
	err1 := s.r.Close()
	err2 := s.w.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// DataDir returns the absolute data directory.
func (s *Store) DataDir() string { return s.dataDir }

// UserVersion returns PRAGMA user_version of the open database.
func (s *Store) UserVersion(ctx context.Context) (int, error) {
	var v int
	if err := s.r.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, internalErr("无法读取数据库版本", err)
	}
	return v, nil
}

// IntegrityCheck runs PRAGMA integrity_check and returns an error describing
// the first problems found.
func (s *Store) IntegrityCheck(ctx context.Context) error {
	rows, err := s.r.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return internalErr("数据库完整性检查失败", err)
	}
	defer rows.Close()
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return internalErr("数据库完整性检查失败", err)
		}
		if line != "ok" {
			problems = append(problems, line)
		}
	}
	if err := rows.Err(); err != nil {
		return internalErr("数据库完整性检查失败", err)
	}
	if len(problems) > 0 {
		return app.E(app.CodeInternal, "数据库已损坏：%s", strings.Join(problems, "; ")).
			WithHint("请从备份恢复数据目录")
	}
	return nil
}

// Write runs fn in one write transaction: it commits when fn returns nil and
// rolls back otherwise (or when fn panics). fn must use only tx; see the
// package comment.
func (s *Store) Write(ctx context.Context, fn func(tx *Tx) error) (err error) {
	sqlTx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return internalErr("无法开始数据库事务", err)
	}
	done := false
	defer func() {
		if !done {
			sqlTx.Rollback()
		}
	}()
	if err := fn(&Tx{Queries: &Queries{s: s, tx: sqlTx}}); err != nil {
		return err
	}
	done = true
	if err := sqlTx.Commit(); err != nil {
		return internalErr("提交数据库事务失败", err)
	}
	return nil
}

// Store returns the store this Queries belongs to (for the file helpers).
func (q *Queries) Store() *Store { return q.s }

// inWrite runs fn inside the current transaction, or inside a new write
// transaction in auto mode.
func (q *Queries) inWrite(ctx context.Context, fn func(q *Queries) error) error {
	if q.tx != nil {
		return fn(q)
	}
	return q.s.Write(ctx, func(tx *Tx) error { return fn(tx.Queries) })
}

// inRead runs fn against one consistent snapshot: the current transaction,
// or a read transaction on the pool in auto mode.
func (q *Queries) inRead(ctx context.Context, fn func(q *Queries) error) error {
	if q.tx != nil {
		return fn(q)
	}
	sqlTx, err := q.s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return internalErr("无法开始数据库读事务", err)
	}
	defer sqlTx.Rollback()
	return fn(&Queries{s: q.s, tx: sqlTx})
}

func (q *Queries) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	var (
		res sql.Result
		err error
	)
	if q.tx != nil {
		res, err = q.tx.ExecContext(ctx, query, args...)
	} else {
		res, err = q.s.w.ExecContext(ctx, query, args...)
	}
	if err != nil {
		return nil, dbErr(err)
	}
	return res, nil
}

func (q *Queries) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if q.tx != nil {
		rows, err = q.tx.QueryContext(ctx, query, args...)
	} else {
		rows, err = q.s.r.QueryContext(ctx, query, args...)
	}
	if err != nil {
		return nil, dbErr(err)
	}
	return rows, nil
}

// scanner is satisfied by *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// queryOne runs a query expected to return at most one row. found is false
// when there is none.
func queryOne[T any](ctx context.Context, q *Queries, scan func(scanner) (T, error), query string, args ...any) (v T, found bool, err error) {
	rows, err := q.query(ctx, query, args...)
	if err != nil {
		return v, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return v, false, dbErr(rows.Err())
	}
	v, err = scan(rows)
	if err != nil {
		return v, false, dbErr(err)
	}
	return v, true, dbErr(rows.Close())
}

func queryAll[T any](ctx context.Context, q *Queries, scan func(scanner) (T, error), query string, args ...any) ([]T, error) {
	rows, err := q.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, dbErr(err)
		}
		out = append(out, v)
	}
	return out, dbErr(rows.Err())
}

// SQLite extended result codes for constraint violations.
const (
	sqliteConstraintPrimaryKey = 1555
	sqliteConstraintUnique     = 2067
)

// IsUniqueViolation reports whether err was caused by a UNIQUE or PRIMARY
// KEY constraint, e.g. a concurrent insert of the same videos.source_key.
func IsUniqueViolation(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code() == sqliteConstraintUnique || se.Code() == sqliteConstraintPrimaryKey
	}
	return false
}

// dbErr converts a driver error into an *app.Error, keeping context errors
// and existing app errors intact.
func dbErr(err error) error {
	if err == nil {
		return nil
	}
	var ae *app.Error
	if errors.As(err, &ae) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if IsUniqueViolation(err) {
		return app.E(app.CodeConflict, "记录已存在").WithCause(err)
	}
	return internalErr("数据库操作失败", err)
}

func internalErr(msg string, err error) *app.Error {
	if err == nil {
		return app.E(app.CodeInternal, "%s", msg)
	}
	return app.E(app.CodeInternal, "%s：%v", msg, err).WithCause(err)
}

func notFound(what, id string) *app.Error {
	return app.E(app.CodeNotFound, "%s不存在：%s", what, id)
}

// nullable maps "" to NULL.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// placeholders returns "?,?,?" for n values.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

func anySlice[T any](in []T) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// clampLimit applies the pagination defaults.
func clampLimit(limit int) int {
	if limit <= 0 {
		return app.DefaultLimit
	}
	return min(limit, app.MaxLimit)
}
