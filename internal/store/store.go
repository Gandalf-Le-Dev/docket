// Package store owns Docket's SQLite database: the todos, the tokens and the
// images pasted into bodies, plus what each close replaced so it can be
// undone. One file, WAL mode, four tables. The whole dataset is a few thousand
// rows forever, so everything here is deliberately boring.
//
// Every write to an item goes through Store.write, which tells subscribers
// (see Subscribe) what changed once the write commits, so no writer, the web
// page or an agent, can change the backlog without open pages hearing of it.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS todo (
  id         INTEGER PRIMARY KEY,
  title      TEXT NOT NULL,
  body       TEXT NOT NULL DEFAULT '',
  scope      TEXT NOT NULL DEFAULT '',
  source     TEXT NOT NULL DEFAULT '',
  via        TEXT NOT NULL,
  state      TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open','done','dropped')),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  closed_at  TEXT
);
CREATE INDEX IF NOT EXISTS todo_state_scope ON todo (state, scope);

CREATE TABLE IF NOT EXISTS token (
  name       TEXT PRIMARY KEY,
  hash       TEXT NOT NULL,
  role       TEXT NOT NULL CHECK (role IN ('publish','review')),
  created_at TEXT NOT NULL,
  revoked_at TEXT
);

-- AUTOINCREMENT so a pruned id is never reused: /image/N is cached as immutable.
CREATE TABLE IF NOT EXISTS image (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  sha256     TEXT NOT NULL UNIQUE,
  mime       TEXT NOT NULL,
  data       BLOB NOT NULL,
  created_at TEXT NOT NULL,
  seen_at    TEXT NOT NULL
);

-- What an item's latest close replaced, so UndoClose can put it back exactly.
-- The id names that close; AUTOINCREMENT so no later close is ever given it.
-- spent says why the close is no longer undoable, see undoLive.
CREATE TABLE IF NOT EXISTS undo (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  todo_id    INTEGER NOT NULL UNIQUE,
  body       TEXT NOT NULL,
  state      TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  closed_at  TEXT,
  at         TEXT NOT NULL,
  spent      INTEGER NOT NULL DEFAULT 0
);
`

// An undo row's spent column. The undo marks its own row apart from any other
// write, so a second click on one Undo can say it already ran.
const (
	undoLive    = 0
	undoChanged = 1 // another write to the item since the close
	undoUndone  = 2 // UndoClose ran
)

// Guard rails on publish: a publish token sits on the least-trusted machine
// in the fleet, so writes are bounded.
const (
	MaxTitleBytes  = 500
	MaxBodyBytes   = 64 * 1024
	MaxScopeBytes  = 100
	MaxSourceBytes = 500
	MaxImageBytes  = 5 << 20
)

var ErrNotFound = errors.New("not found")

// ValidationError marks caller mistakes (too long, bad state, empty title) so
// the MCP layer can report them as tool errors rather than server faults.
type ValidationError string

func (e ValidationError) Error() string { return string(e) }

type Todo struct {
	ID        int64
	Title     string
	Body      string
	Scope     string
	Source    string
	Via       string
	State     string
	CreatedAt string
	UpdatedAt string
	ClosedAt  string // empty while open
}

type ScopeCount struct {
	Scope string
	Open  int
}

type TokenInfo struct {
	Name      string
	Role      string
	CreatedAt string
	Revoked   bool
}

type Store struct {
	db   *sql.DB
	feed feed
}

func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection serializes all writes; at this scale contention is not a
	// problem and SQLITE_BUSY never becomes one either.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	s := &Store{db: db}
	s.feed.boot = bootNonce()
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping() error {
	var one int
	return s.db.QueryRow("SELECT 1").Scan(&one)
}

// clock is swapped by tests that need time to pass without waiting for it.
var clock = time.Now

func now() string { return clock().UTC().Format(time.RFC3339) }

// NormalizeScope is the entire scope taxonomy: trim and lowercase.
func NormalizeScope(scope string) string {
	return strings.ToLower(strings.TrimSpace(scope))
}

func validateTodoFields(title, body, scope, source string) error {
	if strings.TrimSpace(title) == "" {
		return ValidationError("title must not be empty")
	}
	if len(title) > MaxTitleBytes {
		return ValidationError(fmt.Sprintf("title exceeds %d bytes", MaxTitleBytes))
	}
	if len(body) > MaxBodyBytes {
		return ValidationError(fmt.Sprintf("body exceeds %d bytes", MaxBodyBytes))
	}
	if len(scope) > MaxScopeBytes {
		return ValidationError(fmt.Sprintf("scope exceeds %d bytes", MaxScopeBytes))
	}
	if len(source) > MaxSourceBytes {
		return ValidationError(fmt.Sprintf("source exceeds %d bytes", MaxSourceBytes))
	}
	return nil
}

// write runs fn in a transaction and, once it commits, publishes the changes
// fn reports. It is the only way an item is written.
func (s *Store) write(fn func(tx *sql.Tx) ([]Change, error)) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	changes, err := fn(tx)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.feed.publish(changes)
	return nil
}

// AddTodo files an item. If an open item already has the same normalized
// title and scope, the existing id is returned with duplicate=true — agents
// retry, and two agents in one repo notice the same thing. The rule is
// deliberately dumb so it can never eat a genuinely new item.
func (s *Store) AddTodo(title, body, scope, source, via string) (id int64, duplicate bool, err error) {
	title = strings.TrimSpace(title)
	scope = NormalizeScope(scope)
	source = strings.TrimSpace(source)
	if err := validateTodoFields(title, body, scope, source); err != nil {
		return 0, false, err
	}

	err = s.write(func(tx *sql.Tx) ([]Change, error) {
		err := tx.QueryRow(
			"SELECT id FROM todo WHERE state = 'open' AND scope = ? AND lower(title) = lower(?) LIMIT 1",
			scope, title,
		).Scan(&id)
		if err == nil {
			duplicate = true
			return nil, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		ts := now()
		res, err := tx.Exec(
			"INSERT INTO todo (title, body, scope, source, via, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, 'open', ?, ?)",
			title, body, scope, source, via, ts, ts,
		)
		if err != nil {
			return nil, err
		}
		id, err = res.LastInsertId()
		return []Change{{ID: id, Op: Added}}, err
	})
	if err != nil {
		return 0, false, err
	}
	return id, duplicate, nil
}

func scanTodo(row interface{ Scan(...any) error }) (Todo, error) {
	var t Todo
	var closed sql.NullString
	err := row.Scan(&t.ID, &t.Title, &t.Body, &t.Scope, &t.Source, &t.Via, &t.State, &t.CreatedAt, &t.UpdatedAt, &closed)
	if err != nil {
		return Todo{}, err
	}
	t.ClosedAt = closed.String
	return t, nil
}

const todoCols = "id, title, body, scope, source, via, state, created_at, updated_at, closed_at"

// querier is what the database and a transaction share, so one write can be
// part of a larger one.
type querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

func (s *Store) GetTodo(id int64) (Todo, error) { return getTodo(s.db, id) }

func getTodo(q querier, id int64) (Todo, error) {
	t, err := scanTodo(q.QueryRow("SELECT "+todoCols+" FROM todo WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Todo{}, ErrNotFound
	}
	return t, err
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// ListTodos returns items filtered by state ("open", "done", "dropped", or
// "all"), optional scope, and optional substring query over title and body.
// Newest first. No pagination — at this scale there is nothing to paginate.
func (s *Store) ListTodos(state, scope, query string) ([]Todo, error) {
	if state == "" {
		state = "open"
	}
	switch state {
	case "open", "done", "dropped", "all":
	default:
		return nil, ValidationError("state must be one of: open, done, dropped, all")
	}

	where := []string{"1=1"}
	args := []any{}
	if state != "all" {
		where = append(where, "state = ?")
		args = append(args, state)
	}
	if scope != "" {
		where = append(where, "scope = ?")
		args = append(args, NormalizeScope(scope))
	}
	if query != "" {
		like := "%" + escapeLike(query) + "%"
		where = append(where, `(title LIKE ? ESCAPE '\' OR body LIKE ? ESCAPE '\')`)
		args = append(args, like, like)
	}

	rows, err := s.db.Query(
		"SELECT "+todoCols+" FROM todo WHERE "+strings.Join(where, " AND ")+" ORDER BY id DESC",
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var todos []Todo
	for rows.Next() {
		t, err := scanTodo(rows)
		if err != nil {
			return nil, err
		}
		todos = append(todos, t)
	}
	return todos, rows.Err()
}

// Rev names an item's version exactly: any write that changes what it holds
// changes it, even two within the second updated_at counts in.
func (t Todo) Rev() string {
	sum := sha256.New()
	for _, f := range []string{t.Title, t.Body, t.Scope, t.State, t.UpdatedAt, t.ClosedAt} {
		sum.Write([]byte(f))
		sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil)[:8])
}

// TitleRev names the title's version alone, for a form that edits only the
// title and has no business with changes to the rest.
func (t Todo) TitleRev() string {
	sum := sha256.Sum256([]byte(t.Title))
	return hex.EncodeToString(sum[:8])
}

// TodoUpdate carries the fields to change; nil means leave alone. A non-empty
// IfRev makes the update apply only to the item at that Rev, and IfTitleRev
// only to the item with that TitleRev, so a form opened before someone else's
// change cannot overwrite it unseen.
type TodoUpdate struct {
	Title      *string
	Body       *string
	Scope      *string
	State      *string
	IfRev      string
	IfTitleRev string
}

// ChangedError refuses an update whose IfRev or IfTitleRev the item has moved
// past.
type ChangedError struct{ ID int64 }

func (e ChangedError) Error() string {
	return fmt.Sprintf("#%d changed while you were editing it: look it over, then save again", e.ID)
}

func (s *Store) UpdateTodo(id int64, u TodoUpdate) (t Todo, err error) {
	err = s.write(func(tx *sql.Tx) ([]Change, error) {
		before, err := getTodo(tx, id)
		if err != nil {
			return nil, err
		}
		if (u.IfRev != "" && u.IfRev != before.Rev()) || (u.IfTitleRev != "" && u.IfTitleRev != before.TitleRev()) {
			return nil, ChangedError{ID: id}
		}
		t, err = updateTodo(tx, before, u)
		return []Change{{ID: id, Op: stateOp(before.State, t.State)}}, err
	})
	if err != nil {
		return Todo{}, err
	}
	return t, nil
}

// stateOp calls an undo that reopens an item a reopen, not an edit.
func stateOp(before, after string) Op {
	switch {
	case before == "open" && after != "open":
		return Closed
	case before != "open" && after == "open":
		return Reopened
	}
	return Updated
}

func updateTodo(q querier, t Todo, u TodoUpdate) (Todo, error) {
	if u.Title != nil {
		t.Title = strings.TrimSpace(*u.Title)
	}
	if u.Body != nil {
		t.Body = *u.Body
	}
	if u.Scope != nil {
		t.Scope = NormalizeScope(*u.Scope)
	}
	if err := validateTodoFields(t.Title, t.Body, t.Scope, t.Source); err != nil {
		return Todo{}, err
	}

	closed := sql.NullString{String: t.ClosedAt, Valid: t.ClosedAt != ""}
	if u.State != nil {
		switch *u.State {
		case "open":
			closed = sql.NullString{}
		case "done", "dropped":
			if t.State == "open" || t.ClosedAt == "" {
				closed = sql.NullString{String: now(), Valid: true}
			}
		default:
			return Todo{}, ValidationError("state must be one of: open, done, dropped")
		}
		t.State = *u.State
	}
	t.ClosedAt = closed.String
	t.UpdatedAt = now()

	if _, err := q.Exec(
		"UPDATE todo SET title = ?, body = ?, scope = ?, state = ?, updated_at = ?, closed_at = ? WHERE id = ?",
		t.Title, t.Body, t.Scope, t.State, t.UpdatedAt, closed, t.ID,
	); err != nil {
		return Todo{}, err
	}
	_, err := q.Exec("UPDATE undo SET spent = ? WHERE todo_id = ? AND spent = ?", undoChanged, t.ID, undoLive)
	return t, err
}

// CloseTodo records a verdict: done (default) or dropped. Dropped is distinct
// from done because "not going to do this" is worth remembering. A non-empty
// reason is appended to the body so it survives with the item. What the close
// replaced is kept for a while, and undo is the token UndoClose takes to
// reverse this close and no other.
func (s *Store) CloseTodo(id int64, outcome, reason string) (t Todo, undo string, err error) {
	if outcome == "" {
		outcome = "done"
	}
	if outcome != "done" && outcome != "dropped" {
		return Todo{}, "", ValidationError("outcome must be done or dropped")
	}
	err = s.write(func(tx *sql.Tx) ([]Change, error) {
		before, err := getTodo(tx, id)
		if err != nil {
			return nil, err
		}
		body := before.Body
		if reason = strings.TrimSpace(reason); reason != "" {
			if body != "" {
				body += "\n\n"
			}
			body += "— closed (" + outcome + "): " + reason
		}
		t, err = updateTodo(tx, before, TodoUpdate{Body: &body, State: &outcome})
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec("DELETE FROM undo WHERE todo_id = ? OR at < ?", id, undoCutoff()); err != nil {
			return nil, err
		}
		res, err := tx.Exec("INSERT INTO undo (todo_id, body, state, updated_at, closed_at, at) VALUES (?, ?, ?, ?, ?, ?)",
			id, before.Body, before.State, before.UpdatedAt,
			sql.NullString{String: before.ClosedAt, Valid: before.ClosedAt != ""}, t.UpdatedAt)
		if err != nil {
			return nil, err
		}
		n, err := res.LastInsertId()
		undo = strconv.FormatInt(n, 10)
		return []Change{{ID: id, Op: Closed}}, err
	})
	if err != nil {
		return Todo{}, "", err
	}
	return t, undo, nil
}

func undoCutoff() string { return clock().Add(-UndoWindow).UTC().Format(time.RFC3339) }

// UndoWindow is how long a close stays undoable. The web page offers Undo for
// seconds; the rest is slack for a page left open, not an archive.
const UndoWindow = 24 * time.Hour

// UndoClose puts an item back exactly as the close that returned undo found
// it: body, state and both timestamps. Reopening instead keeps the verdict
// note as history. The undo having run already, a later close, any other
// write to the item, or UndoWindow passing leaves that close with nothing to
// undo, and the error says which.
func (s *Store) UndoClose(id int64, undo string) (Todo, error) {
	expired := ValidationError(fmt.Sprintf("nothing to undo on #%d: its undo has expired", id))
	n, err := strconv.ParseInt(undo, 10, 64)
	if err != nil {
		return Todo{}, expired
	}
	var t Todo
	err = s.write(func(tx *sql.Tx) ([]Change, error) {
		var err error
		t, err = getTodo(tx, id)
		if err != nil {
			return nil, err
		}
		was := t.State
		var closed sql.NullString
		var at string
		var spent int
		err = tx.QueryRow("SELECT body, state, updated_at, closed_at, at, spent FROM undo WHERE id = ? AND todo_id = ?", n, id).
			Scan(&t.Body, &t.State, &t.UpdatedAt, &closed, &at, &spent)
		if errors.Is(err, sql.ErrNoRows) {
			// A later close of the item replaced this one's row; otherwise the row
			// was pruned, or the token never named a close of this item.
			var later int
			err = tx.QueryRow("SELECT COUNT(*) FROM undo WHERE todo_id = ?", id).Scan(&later)
			if err == nil && later > 0 {
				return nil, ValidationError(fmt.Sprintf("#%d has been closed again since, so this undo no longer applies", id))
			}
			if err == nil {
				return nil, expired
			}
		}
		switch {
		case err != nil:
			return nil, err
		case at < undoCutoff():
			return nil, expired
		case spent == undoUndone:
			return nil, ValidationError(fmt.Sprintf("#%d is already undone", id))
		case spent != undoLive:
			return nil, ValidationError(fmt.Sprintf("#%d has changed since it was closed, so the close cannot be undone", id))
		}
		t.ClosedAt = closed.String
		if _, err := tx.Exec("UPDATE todo SET body = ?, state = ?, updated_at = ?, closed_at = ? WHERE id = ?",
			t.Body, t.State, t.UpdatedAt, closed, id); err != nil {
			return nil, err
		}
		_, err = tx.Exec("UPDATE undo SET spent = ? WHERE id = ?", undoUndone, n)
		return []Change{{ID: id, Op: stateOp(was, t.State)}}, err
	})
	if err != nil {
		return Todo{}, err
	}
	return t, nil
}

// Scopes lists the scopes in use with their open counts — the entire
// todo_scopes verb.
func (s *Store) Scopes() ([]ScopeCount, error) {
	rows, err := s.db.Query("SELECT scope, COUNT(*) FROM todo WHERE state = 'open' GROUP BY scope ORDER BY COUNT(*) DESC, scope")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScopeCount
	for rows.Next() {
		var sc ScopeCount
		if err := rows.Scan(&sc.Scope, &sc.Open); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// ScopeStats is one scope with a count per state. Unlike Scopes, it includes
// scopes whose items are all closed: the web page's Done and Dropped tabs
// still group closed items under the scope they were filed in.
type ScopeStats struct {
	Scope   string
	Open    int
	Done    int
	Dropped int
	First   int64 // the scope's oldest item id: the order scopes came into use
}

// ScopeSummaries is every scope that has ever held an item, with its counts,
// ordered by name. Unscoped items group under "".
func (s *Store) ScopeSummaries() ([]ScopeStats, error) {
	rows, err := s.db.Query(`SELECT scope,
		SUM(state = 'open'), SUM(state = 'done'), SUM(state = 'dropped'), MIN(id)
		FROM todo GROUP BY scope ORDER BY scope`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScopeStats
	for rows.Next() {
		var st ScopeStats
		if err := rows.Scan(&st.Scope, &st.Open, &st.Done, &st.Dropped, &st.First); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// RenameScope merges scope drift ("hopbox" and "hop-box") in one UPDATE.
func (s *Store) RenameScope(from, to string) (int64, error) {
	from, to = NormalizeScope(from), NormalizeScope(to)
	if len(to) > MaxScopeBytes {
		return 0, ValidationError(fmt.Sprintf("scope exceeds %d bytes", MaxScopeBytes))
	}
	var changes []Change
	err := s.write(func(tx *sql.Tx) ([]Change, error) {
		rows, err := tx.Query("SELECT id FROM todo WHERE scope = ?", from)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			changes = append(changes, Change{ID: id, Op: Updated})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if _, err := tx.Exec("UPDATE undo SET spent = ? WHERE spent = ? AND todo_id IN (SELECT id FROM todo WHERE scope = ?)",
			undoChanged, undoLive, from); err != nil {
			return nil, err
		}
		_, err = tx.Exec("UPDATE todo SET scope = ?, updated_at = ? WHERE scope = ?", to, now(), from)
		return changes, err
	})
	if err != nil {
		return 0, err
	}
	return int64(len(changes)), nil
}

// StateCounts feeds the web UI's tabs.
func (s *Store) StateCounts() (map[string]int, error) {
	rows, err := s.db.Query("SELECT state, COUNT(*) FROM todo GROUP BY state")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		counts[state] = n
	}
	return counts, rows.Err()
}

// --- tokens ---

func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// CreateToken mints a token for a box (publish) or the owner (review) and
// returns the plaintext exactly once; only the hash is stored. Re-creating a
// revoked name rotates it; an active name must be revoked first.
func (s *Store) CreateToken(name, role string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ValidationError("token name must not be empty")
	}
	if role != "publish" && role != "review" {
		return "", ValidationError("role must be publish or review")
	}

	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	plaintext := "dkt_" + hex.EncodeToString(buf)

	var revoked sql.NullString
	err := s.db.QueryRow("SELECT revoked_at FROM token WHERE name = ?", name).Scan(&revoked)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = s.db.Exec("INSERT INTO token (name, hash, role, created_at) VALUES (?, ?, ?, ?)",
			name, hashToken(plaintext), role, now())
	case err != nil:
		return "", err
	case revoked.Valid:
		_, err = s.db.Exec("UPDATE token SET hash = ?, role = ?, created_at = ?, revoked_at = NULL WHERE name = ?",
			hashToken(plaintext), role, now(), name)
	default:
		return "", ValidationError("an active token named " + name + " already exists; revoke it first")
	}
	if err != nil {
		return "", err
	}
	return plaintext, nil
}

func (s *Store) RevokeToken(name string) error {
	res, err := s.db.Exec("UPDATE token SET revoked_at = ? WHERE name = ? AND revoked_at IS NULL", now(), name)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

// Auth resolves a plaintext token to its name and role. The name is what
// makes `via` trustworthy: a box can lie about source, not about its token.
func (s *Store) Auth(plaintext string) (name, role string, err error) {
	err = s.db.QueryRow("SELECT name, role FROM token WHERE hash = ? AND revoked_at IS NULL", hashToken(plaintext)).
		Scan(&name, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return name, role, err
}

func (s *Store) ListTokens() ([]TokenInfo, error) {
	rows, err := s.db.Query("SELECT name, role, created_at, revoked_at IS NOT NULL FROM token ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenInfo
	for rows.Next() {
		var t TokenInfo
		if err := rows.Scan(&t.Name, &t.Role, &t.CreatedAt, &t.Revoked); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// imageTypes is what a body may show. SVG stays out: it is a document that
// can carry script, not a picture.
var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// AddImage stores an image and returns its id. The type is sniffed from the
// bytes, never taken from the uploader. The same bytes twice return the id
// they already have, so pasting a screenshot again costs nothing, and count
// as seeing the image now, which restarts its PruneImages grace.
func (s *Store) AddImage(data []byte) (int64, error) {
	if len(data) > MaxImageBytes {
		return 0, ValidationError(fmt.Sprintf("image exceeds %d MB", MaxImageBytes>>20))
	}
	mime := http.DetectContentType(data)
	if !imageTypes[mime] {
		return 0, ValidationError("image must be PNG, JPEG, GIF or WebP")
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	ts := now()
	if _, err := s.db.Exec(`INSERT INTO image (sha256, mime, data, created_at, seen_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (sha256) DO UPDATE SET seen_at = excluded.seen_at`,
		hash, mime, data, ts, ts); err != nil {
		return 0, err
	}
	var id int64
	err := s.db.QueryRow("SELECT id FROM image WHERE sha256 = ?", hash).Scan(&id)
	return id, err
}

func (s *Store) GetImage(id int64) (mime string, data []byte, err error) {
	err = s.db.QueryRow("SELECT mime, data FROM image WHERE id = ?", id).Scan(&mime, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, ErrNotFound
	}
	return mime, data, err
}

// PruneImages deletes images no body has referenced for grace. The grace runs
// from the last reference a sweep saw, and one transaction keeps a body saved
// mid-sweep from losing an image it has just started to show.
func (s *Store) PruneImages(grace time.Duration) (int, error) {
	if grace <= 0 {
		return 0, ValidationError("grace must be positive")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.Query("SELECT body FROM todo WHERE body LIKE '%/image/%'")
	if err != nil {
		return 0, err
	}
	var refs []int64
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			rows.Close()
			return 0, err
		}
		refs = append(refs, ImageRefs(body)...)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	ts := now()
	for _, id := range refs {
		if _, err := tx.Exec("UPDATE image SET seen_at = ? WHERE id = ?", ts, id); err != nil {
			return 0, err
		}
	}
	// Every referenced image was just seen now, later than the cutoff, so
	// this only reaches unreferenced ones.
	cutoff := clock().Add(-grace).UTC().Format(time.RFC3339)
	res, err := tx.Exec("DELETE FROM image WHERE seen_at < ?", cutoff)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), tx.Commit()
}

// ImageMarker is how a body shows an image: ![alt](/image/<id>). Only this
// relative form counts, so a body can never make a reader fetch a remote URL.
var ImageMarker = regexp.MustCompile(`!\[([^\]\n]*)\]\(/image/(\d+)\)`)

// ImageRefs is the ids of the images a body shows, in order, each once.
func ImageRefs(body string) []int64 {
	var ids []int64
	seen := map[int64]bool{}
	for _, m := range ImageMarker.FindAllStringSubmatch(body, -1) {
		id, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}
