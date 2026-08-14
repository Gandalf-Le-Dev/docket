// Package web is Docket's human surface: one server-rendered page for
// reviewing the backlog from a phone. Plain forms, no build step, no JS.
// Auth is the review token, entered once per device and kept in a cookie.
package web

import (
	"embed"
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Gandalf-Le-Dev/docket/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

const cookieName = "docket_session"

type Handler struct {
	store *store.Store
	tmpl  *template.Template
	mux   *http.ServeMux
}

func NewHandler(s *store.Store) *Handler {
	h := &Handler{
		store: s,
		tmpl: template.Must(template.New("").Funcs(template.FuncMap{
			"shortTime": shortTime,
		}).ParseFS(templateFS, "templates/*.html")),
		mux: http.NewServeMux(),
	}
	h.mux.HandleFunc("GET /login", h.loginForm)
	h.mux.HandleFunc("POST /login", h.login)
	h.mux.HandleFunc("POST /logout", h.logout)
	h.mux.HandleFunc("GET /{$}", h.requireReview(h.index))
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

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
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
		Secure:   secureRequest(r),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type scopeGroup struct {
	Scope string
	Todos []store.Todo
}

type indexData struct {
	State  string
	Scope  string
	Query  string
	Groups []scopeGroup
	Scopes []store.ScopeCount
	Counts map[string]int
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

	byScope := map[string][]store.Todo{}
	for _, t := range todos {
		byScope[t.Scope] = append(byScope[t.Scope], t)
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
		Error:  r.URL.Query().Get("err"),
	})
}

// back redirects to the list view the action came from, carrying any store
// validation message so the page can show it.
func back(w http.ResponseWriter, r *http.Request, err error) {
	q := url.Values{}
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
	dest := "/"
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
	_, _, err := h.store.AddTodo(r.FormValue("title"), r.FormValue("body"), r.FormValue("scope"), "web", via)
	back(w, r, err)
}

func (h *Handler) close(w http.ResponseWriter, r *http.Request) {
	id, err := formID(r)
	if err == nil {
		_, err = h.store.CloseTodo(id, r.FormValue("outcome"), r.FormValue("reason"))
	}
	back(w, r, err)
}

func (h *Handler) reopen(w http.ResponseWriter, r *http.Request) {
	id, err := formID(r)
	if err == nil {
		open := "open"
		_, err = h.store.UpdateTodo(id, store.TodoUpdate{State: &open})
	}
	back(w, r, err)
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id, err := formID(r)
	if err == nil {
		title, body, scope := r.FormValue("title"), r.FormValue("body"), r.FormValue("scope")
		_, err = h.store.UpdateTodo(id, store.TodoUpdate{Title: &title, Body: &body, Scope: &scope})
	}
	back(w, r, err)
}

func (h *Handler) renameScope(w http.ResponseWriter, r *http.Request) {
	_, err := h.store.RenameScope(r.FormValue("from"), r.FormValue("to"))
	back(w, r, err)
}
