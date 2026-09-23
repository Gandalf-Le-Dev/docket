// Package web is Docket's human surface: one server-rendered page for
// reviewing the backlog. Plain forms, no build step, no JS. Every transient
// state is a query parameter the server renders: ?open=<id> puts an entry in
// the drawer, ?new=1 puts the new-entry form there, ?do=edit|drop turns an
// entry into its form in place, ?rename=<scope> does the same to a panel's
// name. Each of those is a normal page load, so back works and URLs share.
//
// The look is the mroc design system: tokens, type and marks come from
// static/app.css, which is copied from that repository rather than invented
// here. Auth is the review token, entered once per device and kept in a cookie.
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
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

//go:embed static
var staticFS embed.FS

// panelCap is how many open entries a scope shows before it offers the rest
// behind a link. One loud project must not push every other scope off screen.
const panelCap = 5

const cookieName = "docket_session"

type Handler struct {
	store    *store.Store
	assetVer string // content hash, so a deploy busts the year-long cache
	tmpl     *template.Template
	mux      *http.ServeMux
}

func NewHandler(s *store.Store) *Handler {
	h := &Handler{
		store:    s,
		assetVer: hashAsset("static/app.css"),
		tmpl: template.Must(template.New("").Funcs(template.FuncMap{
			"shortTime": shortTime,
			"ago":       ago,
			"linkify":   linkify,
			"query":     query,
			"dict":      dict,
		}).ParseFS(templateFS, "templates/*.html")),
		mux: http.NewServeMux(),
	}
	h.mux.Handle("GET /static/", http.StripPrefix("/", cacheStatic(http.FileServer(http.FS(staticFS)))))
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

// cacheStatic lets the fonts and stylesheet be cached hard: they change only
// when the binary does, and the binary is the only thing that serves them.
func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=604800")
		next.ServeHTTP(w, r)
	})
}

// AssetVer is the fingerprint templates append to the stylesheet's URL.
func (h *Handler) AssetVer() string { return h.assetVer }

// hashAsset fingerprints an embedded file so its URL changes when it does.
// Without it the long cache below would serve last week's stylesheet.
func hashAsset(name string) string {
	b, err := staticFS.ReadFile(name)
	if err != nil {
		return "dev"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:4])
}

// dict lets a template pass both an item and the page around it to a
// sub-template, which Go templates otherwise make impossible.
func dict(pairs ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(pairs); i += 2 {
		if k, ok := pairs[i].(string); ok {
			m[k] = pairs[i+1]
		}
	}
	return m
}

// query builds a link back into the list, keeping whichever of state, scope
// and q are set. Templates call it rather than assembling URLs by hand.
func query(pairs ...string) template.URL {
	v := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" && !(pairs[i] == "state" && pairs[i+1] == "open") {
			v.Set(pairs[i], pairs[i+1])
		}
	}
	if len(v) == 0 {
		return template.URL("/")
	}
	return template.URL("/?" + v.Encode())
}

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
// body proper, so a closed item can show its verdict as its own line. It
// returns the outcome the note records as well as its reason: an item that was
// dropped, reopened and then finished carries both notes, and only the one
// matching the item's current state describes it.
func splitVerdict(body string) (text, outcome, reason string) {
	const mark = "— closed ("
	i := strings.LastIndex(body, mark)
	if i < 0 || (i > 0 && !strings.HasSuffix(body[:i], "\n\n")) {
		return body, "", ""
	}
	head, rest, ok := strings.Cut(body[i+len(mark):], "): ")
	if !ok || strings.ContainsAny(head, "\n") {
		return body, "", ""
	}
	return strings.TrimSpace(body[:i]), head, rest
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

// chrome is what every signed-in page carries for its header and tabs, so
// moving between the list and an entry never changes the band's shape.
type chrome struct {
	State  string // the list the header's search and links stay in
	Tab    string // the tab drawn as current
	Scope  string
	Query  string
	Counts map[string]int // per state, for the tabs
	Scopes []store.ScopeStats
	Flash  *flash
	Error  string
	Asset  string // stylesheet fingerprint
}

type loginData struct {
	State string // unused, but "head" and the chrome share one shape
	Error string
	Asset string // stylesheet fingerprint
}

func (h *Handler) loginForm(w http.ResponseWriter, r *http.Request) {
	h.render(w, "login.html", loginData{Asset: h.assetVer, Error: r.URL.Query().Get("err")})
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
		text, outcome, reason := splitVerdict(t.Body)
		// A stale note from an earlier close stays in the body rather than
		// being presented as this item's verdict.
		if outcome == t.State {
			v.Text, v.Verdict = text, reason
		}
	}
	return v
}

// flash is what the page says after an action, with the reverse beside it.
// The Back fields tell the reverse where to land, like any action's form.
type flash struct {
	Text      string
	ID        int64
	Undo      string // form action that reverses it, or ""
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

// panelView is one scope as the page draws it: its counts, the entries that
// fit, and how many did not.
type panelView struct {
	Scope   string
	Open    int
	Done    int
	Dropped int
	Entries []todoView
	More    int
}

// Counts reads "2 open · 12 done · 1 dropped", leaving out the states that are
// empty so a young scope does not carry two zeroes.
func (p panelView) Counts() string {
	var parts []string
	for _, c := range []struct {
		n    int
		name string
	}{{p.Open, "open"}, {p.Done, "done"}, {p.Dropped, "dropped"}} {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c.n, c.name))
		}
	}
	if len(parts) == 0 {
		return "empty"
	}
	return strings.Join(parts, " · ")
}

// PanelCount is how many scopes the page is showing, for the summary line.
func (d indexData) PanelCount() int { return len(d.Left) + len(d.Right) }

func (p panelView) Name() string {
	if p.Scope == "" {
		return "unscoped"
	}
	return p.Scope
}

type indexData struct {
	chrome
	Left   []panelView
	Right  []panelView
	Quiet  []string // scopes with nothing in this state
	Total  int
	Open   *todoView // the drawer's entry, when ?open= named one
	Do     string    // "edit" or "drop": the drawer's entry as that form
	New    bool      // the drawer holds the new-entry form
	In     string    // the scope that form starts in
	Rename string    // the panel whose name is an input, by Name()
}

// pack lays the panels into two columns the way a mason would: tallest first,
// each one onto whichever column is shorter. The browser cannot be trusted to
// balance columns itself once panels may not be split.
func pack(panels []panelView) (left, right []panelView) {
	order := make([]panelView, len(panels))
	copy(order, panels)
	sort.SliceStable(order, func(i, j int) bool { return len(order[i].Entries) > len(order[j].Entries) })
	var lh, rh int
	for _, p := range order {
		// a panel costs its header plus a line per entry, near enough
		cost := 2 + len(p.Entries)
		if lh <= rh {
			left = append(left, p)
			lh += cost
		} else {
			right = append(right, p)
			rh += cost
		}
	}
	sortByScope := func(ps []panelView) {
		sort.SliceStable(ps, func(i, j int) bool {
			if (ps[i].Scope == "") != (ps[j].Scope == "") {
				return ps[j].Scope == ""
			}
			return ps[i].Scope < ps[j].Scope
		})
	}
	sortByScope(left)
	sortByScope(right)
	return left, right
}

func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	scope := store.NormalizeScope(q.Get("scope"))
	search := q.Get("q")

	todos, err := h.store.ListTodos(state, scope, search)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	counts, err := h.store.StateCounts()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	summaries, err := h.store.ScopeSummaries()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	byScope := map[string][]todoView{}
	for _, t := range todos {
		byScope[t.Scope] = append(byScope[t.Scope], newTodoView(t, false, state, scope))
	}

	var panels []panelView
	var quiet []string
	for _, sum := range summaries {
		if scope != "" && sum.Scope != scope {
			continue
		}
		entries := byScope[sum.Scope]
		p := panelView{Scope: sum.Scope, Open: sum.Open, Done: sum.Done, Dropped: sum.Dropped}
		if len(entries) == 0 {
			// A scope with nothing in this state is still a scope. One with
			// nothing at all in it folds into a line at the foot instead.
			if sum.Open+sum.Done+sum.Dropped == 0 || (scope == "" && search == "") {
				quiet = append(quiet, p.Name())
				continue
			}
			panels = append(panels, p)
			continue
		}
		// The cap keeps one loud scope from filling the page. Once you have
		// asked for a single scope, or searched, it would only hide what you
		// asked for — and its "more" link would point back at this same page.
		if scope == "" && search == "" && len(entries) > panelCap {
			p.More = len(entries) - panelCap
			entries = entries[:panelCap]
		}
		p.Entries = entries
		panels = append(panels, p)
	}
	left, right := pack(panels)

	data := indexData{
		chrome: chrome{
			State:  state,
			Tab:    state,
			Scope:  scope,
			Query:  search,
			Counts: counts,
			Scopes: summaries,
			Flash:  flashFrom(q, 0, state, scope),
			Error:  q.Get("err"),
			Asset:  h.assetVer,
		},
		Left:   left,
		Right:  right,
		Quiet:  quiet,
		Total:  len(todos),
		Rename: q.Get("rename"),
	}
	if q.Get("new") != "" {
		data.New = true
		data.In = scope
		if in, ok := q["in"]; ok {
			data.In = store.NormalizeScope(in[0])
		}
	} else if id, err := strconv.ParseInt(q.Get("open"), 10, 64); err == nil && id > 0 {
		if t, err := h.store.GetTodo(id); err == nil {
			v := newTodoView(t, true, state, scope)
			data.Open = &v
			data.Do = mode(q.Get("do"), t.State)
		}
	}
	h.render(w, "index.html", data)
}

type itemData struct {
	chrome
	Item todoView
	Do   string // "edit" or "drop": the entry as that form, in place
}

// mode is the form an entry is showing. Drop only means something while the
// entry is open; a closed one offers Reopen instead.
func mode(do, state string) string {
	switch {
	case do == "edit", do == "drop" && state == "open":
		return do
	}
	return ""
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
	scopes, err := h.store.ScopeSummaries()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	counts, err := h.store.StateCounts()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	h.render(w, "todo.html", itemData{
		chrome: chrome{
			State:  "open",
			Tab:    t.State,
			Counts: counts,
			Scopes: scopes,
			Flash:  flashFrom(q, t.ID, "", ""),
			Error:  q.Get("err"),
			Asset:  h.assetVer,
		},
		Item: newTodoView(t, true, "", ""),
		Do:   mode(q.Get("do"), t.State),
	})
}

// back redirects to where the action came from — the item's own page when
// back_id is set, otherwise the list view, with back_open's entry in the
// drawer — carrying either what was done (see flashFrom) or the store's
// validation message, so the page can say so. A failed form comes back
// still open (back_do, back_new), so the message lands beside it.
func back(w http.ResponseWriter, r *http.Request, err error, did url.Values) {
	q := url.Values{}
	if err == nil {
		q = did
	}
	dest := "/"
	if id, e := strconv.ParseInt(r.FormValue("back_id"), 10, 64); e == nil {
		dest = fmt.Sprintf("/todo/%d", id)
	}
	for _, k := range []string{"state", "scope", "q", "open"} {
		if s := r.FormValue("back_" + k); s != "" && !(k == "state" && s == "open") && q.Get(k) == "" {
			q.Set(k, s)
		}
	}
	if err != nil {
		if s := r.FormValue("back_do"); s != "" {
			q.Set("do", s)
		}
		if r.FormValue("back_new") != "" {
			q.Set("new", "1")
			q.Set("in", r.FormValue("scope"))
		}
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
	// the drawer that held the form now holds what it filed
	back(w, r, err, did("added", id, "duplicate", strconv.FormatBool(dup), "open", strconv.FormatInt(id, 10)))
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
	from, to := r.FormValue("from"), r.FormValue("to")
	_, err := h.store.RenameScope(from, to)
	// A list filtered to the old name would come back empty.
	if err == nil && store.NormalizeScope(r.FormValue("back_scope")) == store.NormalizeScope(from) {
		r.Form.Set("back_scope", store.NormalizeScope(to))
	}
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
