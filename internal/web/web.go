// Package web is Docket's human surface: one server-rendered page for
// reviewing the backlog from a phone. Plain forms, no build step, no JS.
// Auth is the review token, entered once per device and kept in a cookie.
package web

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Gandalf-Le-Dev/docket/internal/link"
	"github.com/Gandalf-Le-Dev/docket/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

const cookieName = "docket_session"

type Handler struct {
	store     *store.Store
	publicURL string // see link.Base
	tmpl      *template.Template
	mux       *http.ServeMux
}

func NewHandler(s *store.Store, publicURL string) *Handler {
	h := &Handler{
		store:     s,
		publicURL: publicURL,
		tmpl: template.Must(template.New("").Funcs(template.FuncMap{
			"shortTime": shortTime,
			"ago":       ago,
			"linkify":   linkify,
		}).ParseFS(templateFS, "templates/*.html")),
		mux: http.NewServeMux(),
	}
	h.mux.HandleFunc("GET /login", h.loginForm)
	h.mux.HandleFunc("POST /login", h.login)
	h.mux.HandleFunc("POST /logout", h.logout)
	h.mux.HandleFunc("GET /{$}", h.requireReview(h.index))
	h.mux.HandleFunc("GET /todo/{id}", h.requireReview(h.item))
	h.mux.HandleFunc("POST /add", h.requireReview(h.add))
	h.mux.HandleFunc("POST /todo/close", h.requireReview(h.close))
	h.mux.HandleFunc("POST /todo/reopen", h.requireReview(h.reopen))
	h.mux.HandleFunc("POST /todo/update", h.requireReview(h.update))
	h.mux.HandleFunc("POST /scope/rename", h.requireReview(h.renameScope))
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

func shortTime(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	return t.Format("Jan 2, 2006 15:04")
}

// ago is how old an item reads at a glance; the exact time rides in a title
// attribute next to it.
func ago(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.Format("Jan 2, 2006")
}

// splitVerdict separates the closing note CloseTodo appends to a body from the
// body proper, so a closed item can show its verdict as its own line.
func splitVerdict(body string) (text, verdict string) {
	const mark = "— closed ("
	i := strings.LastIndex(body, mark)
	if i < 0 || (i > 0 && !strings.HasSuffix(body[:i], "\n\n")) {
		return body, ""
	}
	_, reason, ok := strings.Cut(body[i:], "): ")
	if !ok {
		return body, ""
	}
	return strings.TrimSpace(body[:i]), reason
}

var urlRE = regexp.MustCompile(`https?://[^\s<>"']+`)

// linkify escapes body text and turns bare URLs into links, so a PR or issue
// mentioned in an item is one tap away without links being a field of their
// own. Trailing punctuation stays outside the link; a closing paren is kept
// only when the URL opened one.
func linkify(s string) template.HTML {
	var b strings.Builder
	last := 0
	for _, m := range urlRE.FindAllStringIndex(s, -1) {
		start, end := m[0], m[1]
		for end > start {
			c := s[end-1]
			if c == ')' && strings.Count(s[start:end], "(") >= strings.Count(s[start:end], ")") {
				break
			}
			if !strings.ContainsRune(".,;:!?)", rune(c)) {
				break
			}
			end--
		}
		b.WriteString(template.HTMLEscapeString(s[last:start]))
		u := template.HTMLEscapeString(s[start:end])
		fmt.Fprintf(&b, `<a href="%s" rel="noopener">%s</a>`, u, u)
		last = end
	}
	b.WriteString(template.HTMLEscapeString(s[last:]))
	return template.HTML(b.String())
}

func (h *Handler) render(w http.ResponseWriter, name string, data any) {
	if err := h.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("web: render %s: %v", name, err)
	}
}

// requireReview gates every page behind the review token. Publish tokens are
// rejected the same as garbage: the web surface is read/write, and publish
// must never read.
func (h *Handler) requireReview(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err == nil {
			if _, role, err := h.store.Auth(c.Value); err == nil && role == "review" {
				next(w, r)
				return
			}
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

type loginData struct {
	Error string
}

func (h *Handler) loginForm(w http.ResponseWriter, r *http.Request) {
	h.render(w, "login.html", loginData{Error: r.URL.Query().Get("err")})
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.FormValue("token"))
	_, role, err := h.store.Auth(token)
	if err != nil || role != "review" {
		msg := "that token is not valid"
		if err == nil {
			msg = "that is a publish token; the web page needs a review token"
		}
		http.Redirect(w, r, "/login?err="+url.QueryEscape(msg), http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   365 * 24 * 60 * 60,
		HttpOnly: true,
		Secure:   link.HTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// todoView is one item as the "item" template renders it. Focus marks the
// item's own page: opened, and actions return there rather than to the list.
type todoView struct {
	store.Todo
	Text      string // Body without the closing note
	Verdict   string // the closing note's reason, for closed items
	Focus     bool
	BackState string
	BackScope string
}

func newTodoView(t store.Todo, focus bool, backState, backScope string) todoView {
	v := todoView{Todo: t, Text: t.Body, Focus: focus, BackState: backState, BackScope: backScope}
	if t.State != "open" {
		v.Text, v.Verdict = splitVerdict(t.Body)
	}
	return v
}

// flash is what the page says after an action, with the reverse beside it.
// The Back fields tell the reverse where to land, like any action's form.
type flash struct {
	Text      string
	ID        int64
	Undo      string // form action that reverses it, or ""
	Link      string // an item to open, or ""
	BackID    int64
	BackState string
	BackScope string
}

func flashFrom(q url.Values, backID int64, backState, backScope string) *flash {
	id, _ := strconv.ParseInt(q.Get("id"), 10, 64)
	f := &flash{ID: id, BackID: backID, BackState: backState, BackScope: backScope}
	switch q.Get("did") {
	case "closed":
		f.Undo = "/todo/reopen"
		if q.Get("outcome") == "dropped" {
			f.Text = fmt.Sprintf("Dropped #%d.", id)
		} else {
			f.Text = fmt.Sprintf("Closed #%d as done.", id)
		}
	case "reopened":
		f.Text = fmt.Sprintf("Reopened #%d.", id)
	case "added":
		f.Link = fmt.Sprintf("/todo/%d", id)
		if q.Get("duplicate") == "true" {
			f.Text = fmt.Sprintf("Already open as #%d.", id)
		} else {
			f.Text = fmt.Sprintf("Filed #%d.", id)
		}
	case "saved":
		f.Text = fmt.Sprintf("Saved #%d.", id)
	case "renamed":
		f.Text = "Scope renamed."
	default:
		return nil
	}
	return f
}

type scopeGroup struct {
	Scope string
	Todos []todoView
}

type indexData struct {
	State  string
	Scope  string
	Query  string
	Groups []scopeGroup
	Scopes []store.ScopeCount
	Counts map[string]int
	Flash  *flash
	Error  string
}

func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	scope := r.URL.Query().Get("scope")
	query := r.URL.Query().Get("q")

	todos, err := h.store.ListTodos(state, scope, query)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	counts, err := h.store.StateCounts()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	scopes, err := h.store.Scopes()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	byScope := map[string][]todoView{}
	for _, t := range todos {
		byScope[t.Scope] = append(byScope[t.Scope], newTodoView(t, false, state, scope))
	}
	names := make([]string, 0, len(byScope))
	for name := range byScope {
		names = append(names, name)
	}
	// Alphabetical, with unscoped items last rather than first.
	sort.Slice(names, func(i, j int) bool {
		if (names[i] == "") != (names[j] == "") {
			return names[j] == ""
		}
		return names[i] < names[j]
	})
	groups := make([]scopeGroup, 0, len(names))
	for _, name := range names {
		groups = append(groups, scopeGroup{Scope: name, Todos: byScope[name]})
	}

	h.render(w, "index.html", indexData{
		State:  state,
		Scope:  scope,
		Query:  query,
		Groups: groups,
		Scopes: scopes,
		Counts: counts,
		Flash:  flashFrom(r.URL.Query(), 0, state, scope),
		Error:  r.URL.Query().Get("err"),
	})
}

type itemData struct {
	Item   todoView
	URL    string
	Scopes []store.ScopeCount
	Flash  *flash
	Error  string
}

// item is an item's own page — the target of its url — with the same
// actions as the list.
func (h *Handler) item(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	t, err := h.store.GetTodo(id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	scopes, err := h.store.Scopes()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.render(w, "todo.html", itemData{
		Item:   newTodoView(t, true, "", ""),
		URL:    link.Item(link.Base(r, h.publicURL), t.ID),
		Scopes: scopes,
		Flash:  flashFrom(r.URL.Query(), t.ID, "", ""),
		Error:  r.URL.Query().Get("err"),
	})
}

// back redirects to where the action came from — the item's own page when
// back_id is set, otherwise the list view — carrying either what was done
// (see flashFrom) or the store's validation message, so the page can say so.
func back(w http.ResponseWriter, r *http.Request, err error, did url.Values) {
	q := url.Values{}
	if err == nil {
		q = did
	}
	dest := "/"
	if id, e := strconv.ParseInt(r.FormValue("back_id"), 10, 64); e == nil {
		dest = fmt.Sprintf("/todo/%d", id)
	}
	if s := r.FormValue("back_state"); s != "" && s != "open" {
		q.Set("state", s)
	}
	if s := r.FormValue("back_scope"); s != "" {
		q.Set("scope", s)
	}
	if err != nil {
		var ve store.ValidationError
		switch {
		case errors.As(err, &ve):
			q.Set("err", ve.Error())
		case errors.Is(err, store.ErrNotFound):
			q.Set("err", "no such item")
		default:
			q.Set("err", "storage failure")
		}
	}
	if len(q) > 0 {
		dest += "?" + q.Encode()
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func formID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		return 0, store.ErrNotFound
	}
	return id, nil
}

func (h *Handler) add(w http.ResponseWriter, r *http.Request) {
	// The owner is also allowed to have ideas. `via` records the reviewer's
	// token name, same as any other writer.
	c, _ := r.Cookie(cookieName)
	via, _, _ := h.store.Auth(c.Value)
	id, dup, err := h.store.AddTodo(r.FormValue("title"), r.FormValue("body"), r.FormValue("scope"), "web", via)
	back(w, r, err, did("added", id, "duplicate", strconv.FormatBool(dup)))
}

func (h *Handler) close(w http.ResponseWriter, r *http.Request) {
	id, err := formID(r)
	outcome := r.FormValue("outcome")
	if err == nil {
		var t store.Todo
		t, err = h.store.CloseTodo(id, outcome, r.FormValue("reason"))
		outcome = t.State
	}
	back(w, r, err, did("closed", id, "outcome", outcome))
}

func (h *Handler) reopen(w http.ResponseWriter, r *http.Request) {
	id, err := formID(r)
	if err == nil {
		open := "open"
		_, err = h.store.UpdateTodo(id, store.TodoUpdate{State: &open})
	}
	back(w, r, err, did("reopened", id))
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id, err := formID(r)
	if err == nil {
		title, body, scope := r.FormValue("title"), r.FormValue("body"), r.FormValue("scope")
		_, err = h.store.UpdateTodo(id, store.TodoUpdate{Title: &title, Body: &body, Scope: &scope})
	}
	back(w, r, err, did("saved", id))
}

func (h *Handler) renameScope(w http.ResponseWriter, r *http.Request) {
	_, err := h.store.RenameScope(r.FormValue("from"), r.FormValue("to"))
	back(w, r, err, did("renamed", 0))
}

// did describes a completed action for back: what happened, to which item,
// plus any extra key/value pairs.
func did(what string, id int64, kv ...string) url.Values {
	q := url.Values{"did": {what}}
	if id != 0 {
		q.Set("id", strconv.FormatInt(id, 10))
	}
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	return q
}
