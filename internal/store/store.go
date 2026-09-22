// Package store owns Docket's SQLite database: the todos and the tokens.
// One file, WAL mode, two tables. The whole dataset is a few thousand rows
// forever, so everything here is deliberately boring.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
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
`

// Guard rails on publish: a publish token sits on the least-trusted machine
// in the fleet, so writes are bounded.
const (
	MaxTitleBytes  = 500
	MaxBodyBytes   = 64 * 1024
	MaxScopeBytes  = 100
	MaxSourceBytes = 500
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
	db *sql.DB
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
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping() error {
	var one int
	return s.db.QueryRow("SELECT 1").Scan(&one)
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

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

	err = s.db.QueryRow(
		"SELECT id FROM todo WHERE state = 'open' AND scope = ? AND lower(title) = lower(?) LIMIT 1",
		scope, title,
	).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}

	ts := now()
	res, err := s.db.Exec(
		"INSERT INTO todo (title, body, scope, source, via, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, 'open', ?, ?)",
		title, body, scope, source, via, ts, ts,
	)
	if err != nil {
		return 0, false, err
	}
	id, err = res.LastInsertId()
	return id, false, err
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

func (s *Store) GetTodo(id int64) (Todo, error) {
	t, err := scanTodo(s.db.QueryRow("SELECT "+todoCols+" FROM todo WHERE id = ?", id))
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

// TodoUpdate carries the fields to change; nil means leave alone.
type TodoUpdate struct {
	Title *string
	Body  *string
	Scope *string
	State *string
}

func (s *Store) UpdateTodo(id int64, u TodoUpdate) (Todo, error) {
	t, err := s.GetTodo(id)
	if err != nil {
		return Todo{}, err
	}
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

	_, err = s.db.Exec(
		"UPDATE todo SET title = ?, body = ?, scope = ?, state = ?, updated_at = ?, closed_at = ? WHERE id = ?",
		t.Title, t.Body, t.Scope, t.State, t.UpdatedAt, closed, t.ID,
	)
	return t, err
}

// CloseTodo records a verdict: done (default) or dropped. Dropped is distinct
// from done because "not going to do this" is worth remembering. A non-empty
// reason is appended to the body so it survives with the item.
func (s *Store) CloseTodo(id int64, outcome, reason string) (Todo, error) {
	if outcome == "" {
		outcome = "done"
	}
	if outcome != "done" && outcome != "dropped" {
		return Todo{}, ValidationError("outcome must be done or dropped")
	}
	t, err := s.GetTodo(id)
	if err != nil {
		return Todo{}, err
	}
	body := t.Body
	if reason = strings.TrimSpace(reason); reason != "" {
		if body != "" {
			body += "\n\n"
		}
		body += "— closed (" + outcome + "): " + reason
	}
	return s.UpdateTodo(id, TodoUpdate{Body: &body, State: &outcome})
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
// scopes whose items are all closed — the web page lists those too, so that a
// project you finished does not simply vanish.
type ScopeStats struct {
	Scope   string
	Open    int
	Done    int
	Dropped int
}

// ScopeSummaries is every scope that has ever held an item, with its counts,
// ordered by name. Unscoped items group under "".
func (s *Store) ScopeSummaries() ([]ScopeStats, error) {
	rows, err := s.db.Query(`SELECT scope,
		SUM(state = 'open'), SUM(state = 'done'), SUM(state = 'dropped')
		FROM todo GROUP BY scope ORDER BY scope`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScopeStats
	for rows.Next() {
		var st ScopeStats
		if err := rows.Scan(&st.Scope, &st.Open, &st.Done, &st.Dropped); err != nil {
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
	res, err := s.db.Exec("UPDATE todo SET scope = ?, updated_at = ? WHERE scope = ?", to, now(), from)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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
