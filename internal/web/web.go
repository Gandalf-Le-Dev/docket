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
	Focus     bool
	BackState string
	BackScope string
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
		byScope[t.Scope] = append(byScope[t.Scope], todoView{Todo: t, BackState: state, BackScope: scope})
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

type itemData struct {
	Item  todoView
	URL   string
	Error string
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
	h.render(w, "todo.html", itemData{
		Item:  todoView{Todo: t, Focus: true},
		URL:   link.Item(link.Base(r, h.publicURL), t.ID),
		Error: r.URL.Query().Get("err"),
	})
}

// back redirects to where the action came from — the item's own page when
// back_id is set, otherwise the list view — carrying any store validation
// message so the page can show it.
func back(w http.ResponseWriter, r *http.Request, err error) {
	q := url.Values{}
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
