// Package web is Docket's human surface: one server-rendered page for
// reviewing the backlog. Plain links and forms, no build step. Every transient
// state is a query parameter the server renders: ?open=<id> puts an entry in
// the drawer, ?new=1 puts the new-entry form there, ?do=edit|drop turns an
// entry into its form in place, ?rename=<scope> does the same to a panel's
// name. Each of those URLs renders the whole page, so back works and URLs
// share.
//
// htmx (static/htmx.min.js, vendored) boosts those links and forms: it fetches
// the same whole page, swaps in its #page and pushes the URL, so a click never
// reloads and, where hx-swap says show:none, keeps the list's scroll. The
// server renders no fragments; the one thing it does for htmx is send a
// signed-out request to the login page whole (see toLogin). What an action
// did shows as a toast, carried out of band into a live region outside #page;
// Done's and Drop's carry an Undo that puts the entry back exactly, verdict
// note and all (store.UndoClose). The page's own script, static/app.js,
// uploads images pasted or dropped into a body, and keeps an open page
// current: /events streams the store's change feed, and on a change the
// script fetches the page's own URL again and swaps its #page like a boosted
// link, holding off while a form is being filled in. It also runs the search
// as it is typed, turns a clicked title into a field that posts the title
// alone, keeps form drafts in the browser until Sign out, and takes an
// action's report (?did=...) out of the address bar once its toast shows.
// Without either script every link and form still works as a page load.
//
// The look is the mroc design system: tokens, type and marks come from
// static/app.css, which is copied from that repository rather than invented
// here. Auth is the review token, entered once per device and kept in a cookie
// until Sign out, in the header's theme menu, clears it.
package web

import (
	"cmp"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"math/rand/v2"
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

const cookieName = "docket_session"

// View preferences live in cookies per browser and never reach the store.
const (
	densityCookie = "docket_density" // "compact", or unset for detailed rows
	themeCookie   = "docket_theme"   // "light" or "dark", or unset to follow the OS
)

// hues is how many scope colors app.css defines (--hue-0 and on).
const hues = 8

type Handler struct {
	store    *store.Store
	assetVer string // content hash, so a deploy busts the year-long cache
	tmpl     *template.Template
	mux      *http.ServeMux
	serve    http.Handler  // mux, behind the cross-origin check
	beat     time.Duration // heartbeat, shortened by tests
}

func NewHandler(s *store.Store) *Handler {
	h := &Handler{
		store:    s,
		assetVer: hashAsset(fingerprinted...),
		tmpl: template.Must(template.New("").Funcs(template.FuncMap{
			"shortTime": shortTime,
			"ago":       ago,
			"body":      renderBody,
			"excerpt":   excerpt,
			"query":     query,
			"dict":      dict,
		}).ParseFS(templateFS, "templates/*.html")),
		mux:  http.NewServeMux(),
		beat: heartbeat,
	}
	h.mux.Handle("GET /static/", http.StripPrefix("/", cacheStatic(http.FileServer(http.FS(staticFS)))))
	h.mux.HandleFunc("GET /login", h.loginForm)
	h.mux.HandleFunc("POST /login", h.login)
	h.mux.HandleFunc("POST /logout", h.logout)
	h.mux.HandleFunc("GET /{$}", h.requireReview(h.index))
	h.mux.HandleFunc("GET /todo/{id}", h.requireReview(h.item))
	h.mux.HandleFunc("GET /events", h.events)
	h.mux.HandleFunc("POST /add", h.requireReview(h.add))
	h.mux.HandleFunc("POST /todo/close", h.requireReview(h.close))
	h.mux.HandleFunc("POST /todo/reopen", h.requireReview(h.reopen))
	h.mux.HandleFunc("POST /todo/undo", h.requireReview(h.undo))
	h.mux.HandleFunc("POST /todo/update", h.requireReview(h.update))
	h.mux.HandleFunc("POST /scope/rename", h.requireReview(h.renameScope))
	h.mux.HandleFunc("POST /image", h.requireReview(h.uploadImage))
	h.mux.HandleFunc("GET /image/{id}", h.requireReview(h.image))
	h.mux.HandleFunc("POST /density", h.requireReview(pref(densityCookie, "compact")))
	h.mux.HandleFunc("POST /theme", pref(themeCookie, "light", "dark"))
	h.serve = crossOriginGuard().Handler(h.mux)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.serve.ServeHTTP(w, r) }

// crossOriginGuard refuses every post another origin makes. The session
// cookie is SameSite=Lax, which still lets a sibling subdomain post here, and
// only this site's own pages and script have any business doing so. The
// image upload answers its script in JSON, refusals included.
func crossOriginGuard() *http.CrossOriginProtection {
	c := http.NewCrossOriginProtection()
	c.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/image" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin upload refused"})
			return
		}
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
	}))
	return c
}

// cacheStatic lets the fonts, stylesheet and scripts be cached hard: they
// change only when the binary does, and the binary is the only thing that
// serves them.
func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=604800")
		next.ServeHTTP(w, r)
	})
}

// AssetVer is the fingerprint templates append to the stylesheet's and the
// scripts' URLs.
func (h *Handler) AssetVer() string { return h.assetVer }

var fingerprinted = []string{"static/app.css", "static/app.js", "static/htmx.min.js"}

// hashAsset fingerprints embedded files so their URLs change when any does.
// Without it the long cache below would serve last week's stylesheet.
func hashAsset(names ...string) string {
	sum := sha256.New()
	for _, name := range names {
		b, err := staticFS.ReadFile(name)
		if err != nil {
			return "dev"
		}
		sum.Write(b)
	}
	return hex.EncodeToString(sum.Sum(nil)[:4])
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

// scopeHues gives each scope a color class by the order it came into use:
// the first eight scopes never share one, and a new scope never repaints an
// old one. Unscoped work gets none.
func scopeHues(sums []store.ScopeStats) map[string]string {
	order := make([]store.ScopeStats, 0, len(sums))
	for _, sum := range sums {
		if sum.Scope != "" {
			order = append(order, sum)
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i].First < order[j].First })
	m := make(map[string]string, len(order))
	for i, sum := range order {
		m[sum.Scope] = fmt.Sprintf("hue-%d", i%hues)
	}
	return m
}

// winks schedules the wordmark's three winks for one page load, as the CSS
// custom properties the animation reads. The server rolls them so the page
// needs no script, and every page load rolls again, so they never fall into
// a rhythm.
func winks() template.CSS {
	w1 := 8 + rand.Float64()*32
	w2 := w1 + 30 + rand.Float64()*90
	w3 := w2 + 60 + rand.Float64()*180
	return template.CSS(fmt.Sprintf("--w1: %.1fs; --w2: %.1fs; --w3: %.1fs", w1, w2, w3))
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
func ago(rfc3339 string) string { return agoAt(rfc3339, time.Now()) }

// agoAt's rules are ported to ago in static/app.js, which keeps a page's
// times current without a reload: change both together.
func agoAt(rfc3339 string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	d := now.Sub(t)
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

// renderBody is how a body reads: linkified text, with each image marker
// drawn as the image it points to. Any other ![..](..) stays text, so a body
// can never make a reader's browser fetch a remote image.
func renderBody(s string) template.HTML {
	var b strings.Builder
	last := 0
	for _, m := range store.ImageMarker.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(string(linkify(s[last:m[0]])))
		src := "/image/" + s[m[4]:m[5]]
		fmt.Fprintf(&b, `<a href="%s" target="_blank" rel="noopener"><img src="%s" alt="%s" loading="lazy"></a>`,
			src, src, template.HTMLEscapeString(s[m[2]:m[3]]))
		last = m[1]
	}
	b.WriteString(string(linkify(s[last:])))
	return template.HTML(b.String())
}

// excerpt is text shown on one line, where an image cannot fit: each marker
// becomes a word saying one is there.
func excerpt(s string) string {
	return store.ImageMarker.ReplaceAllString(s, "[image]")
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
		if h.signedIn(cookie(r, cookieName)) {
			next(w, r)
			return
		}
		toLogin(w, r)
	}
}

func (h *Handler) signedIn(token string) bool {
	_, role, err := h.store.Auth(token)
	return err == nil && role == "review"
}

// toLogin sends a signed-out browser to the login page. htmx would follow a
// plain redirect and swap the login page into the one it came from, so an htmx
// request is told to load it whole instead. A back-button restore is the one
// htmx request that ignores HX-Redirect: it follows the 303 and puts the login
// page in the body, which works because the login form is never boosted.
func toLogin(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-History-Restore-Request") != "true" {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// chrome is what every signed-in page carries for its header and tabs, so
// moving between the list and an entry never changes the band's shape.
type chrome struct {
	State   string // the list the header's search and links stay in
	Tab     string // the tab drawn as current
	Scope   string
	Query   string
	Counts  map[string]int // per state, for the tabs
	Scopes  []store.ScopeStats
	Flash   *flash
	Error   string
	Asset   string // stylesheet and scripts fingerprint
	Seq     string // store.Seq as of this render, for the live refresh
	Here    string // this page's URL less its report (see here), for the view switches to return to
	List    bool   // a list, so the density switch applies
	Compact bool
	Theme   string            // data-theme, or "" to follow the OS
	Hues    map[string]string // scope to color class
	Wink    template.CSS      // when the wordmark winks, see winks
}

type loginData struct {
	Error string
	Asset string // stylesheet and scripts fingerprint
	Theme string
	Wink  template.CSS
}

func (h *Handler) loginForm(w http.ResponseWriter, r *http.Request) {
	h.render(w, "login.html", loginData{Asset: h.assetVer, Theme: theme(r), Wink: winks(), Error: r.URL.Query().Get("err")})
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
	toLogin(w, r)
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

// flash is what the page says after an action, as a toast, with the reverse
// beside it. The Back fields tell the reverse where to land, like any
// action's form.
type flash struct {
	Text      string
	ID        int64
	Undo      string // form action that reverses it, or ""
	Token     string // which close Undo reverses, see store.UndoClose
	BackID    int64
	BackState string
	BackScope string
	BackQuery string
}

func flashFrom(q url.Values, backID int64, backState, backScope, backQuery string) *flash {
	id, _ := strconv.ParseInt(q.Get("id"), 10, 64)
	f := &flash{ID: id, BackID: backID, BackState: backState, BackScope: backScope, BackQuery: backQuery}
	switch q.Get("did") {
	case "closed":
		f.Undo, f.Token = "/todo/undo", q.Get("undo")
		if q.Get("outcome") == "dropped" {
			f.Text = "Dropped."
		} else {
			f.Text = "Done."
		}
	case "restored":
		f.Text = fmt.Sprintf("Restored #%d.", id)
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

// panelView is one scope as the page draws it: its counts and its entries.
type panelView struct {
	Scope   string
	Open    int
	Done    int
	Dropped int
	Entries []todoView
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
	Total  int
	Open   *todoView // the drawer's entry, when ?open= named one
	Do     string    // "edit" or "drop": the drawer's entry as that form
	Edit   *editView // what that form shows in place of the entry's fields
	New    bool      // the drawer holds the new-entry form
	In     string    // the scope that form starts in
	Rename string    // the panel whose name is an input, by Name()
}

// editView is an edit form whose save was refused because the entry moved on
// (see update): it shows what was typed, not the entry's fields, and beside
// each field that differs, what the entry holds now. The page is rendered in
// the refusal's answer, so the typed text survives without script or storage.
type editView struct {
	Title, Body, Scope string
	Error              string
	now                map[string]string
}

// Field is what a form field shows: the typed text, or the entry's own.
func (e *editView) Field(name, saved string) string {
	if e == nil {
		return saved
	}
	return map[string]string{"title": e.Title, "body": e.Body, "scope": e.Scope}[name]
}

// Now is the entry's value of a field that differs from what was typed, or
// "" where they agree.
func (e *editView) Now(name string) string {
	if e == nil {
		return ""
	}
	return e.now[name]
}

// newEditView merges what r posted into cur. The form posts each field twice,
// as typed and as it was when the form opened (orig_<field>): a field the
// user changed keeps the typed text, one they left takes cur's, and a field
// someone else changed meanwhile is noted with cur's value. A form that does
// not post a field's original shows the typed text, noted wherever cur
// differs from it.
func newEditView(cur store.Todo, r *http.Request, reason string) *editView {
	e := &editView{Title: cur.Title, Body: cur.Body, Scope: cur.Scope, Error: reason, now: map[string]string{}}
	lines := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
	for _, f := range []struct {
		name   string
		field  *string
		same   func(string) string
		server string
	}{
		{"title", &e.Title, strings.TrimSpace, cur.Title},
		{"body", &e.Body, lines, lines(cur.Body)},
		{"scope", &e.Scope, store.NormalizeScope, cur.Scope},
	} {
		typed, orig := posted(r, f.name), posted(r, "orig_"+f.name)
		switch {
		case typed == nil:
			continue
		case orig == nil:
			*f.field = *typed
			if f.same(*typed) != f.server {
				e.now[f.name] = cmp.Or(f.server, "(empty)")
			}
			continue
		}
		if f.same(*typed) != f.same(*orig) {
			*f.field = *typed
		}
		if f.same(*orig) != f.server {
			e.now[f.name] = cmp.Or(f.server, "(empty)")
		}
	}
	return e
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

func (h *Handler) index(w http.ResponseWriter, r *http.Request) { h.indexPage(w, r, nil) }

func (h *Handler) indexPage(w http.ResponseWriter, r *http.Request, edit *editView) {
	seq := h.store.Seq()
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
	for _, sum := range summaries {
		entries := byScope[sum.Scope]
		if len(entries) == 0 || (scope != "" && sum.Scope != scope) {
			continue
		}
		panels = append(panels, panelView{Scope: sum.Scope, Open: sum.Open, Done: sum.Done,
			Dropped: sum.Dropped, Entries: entries})
	}
	left, right := pack(panels)

	data := indexData{
		chrome: chrome{
			State:   state,
			Tab:     state,
			Scope:   scope,
			Query:   search,
			Counts:  counts,
			Scopes:  summaries,
			Hues:    scopeHues(summaries),
			Flash:   flashFrom(q, 0, state, scope, search),
			Error:   q.Get("err"),
			Asset:   h.assetVer,
			Seq:     seq,
			Here:    here(r),
			List:    true,
			Compact: cookie(r, densityCookie) == "compact",
			Theme:   theme(r),
			Wink:    winks(),
		},
		Left:   left,
		Right:  right,
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
	if edit != nil {
		data.Edit, data.Error = edit, edit.Error
	}
	h.render(w, "index.html", data)
}

type itemData struct {
	chrome
	Item todoView
	Do   string    // "edit" or "drop": the entry as that form, in place
	Edit *editView // what that form shows in place of the entry's fields
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
func (h *Handler) item(w http.ResponseWriter, r *http.Request) { h.itemPage(w, r, nil) }

func (h *Handler) itemPage(w http.ResponseWriter, r *http.Request, edit *editView) {
	seq := h.store.Seq()
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
	data := itemData{
		chrome: chrome{
			State:  "open",
			Tab:    t.State,
			Counts: counts,
			Scopes: scopes,
			Hues:   scopeHues(scopes),
			Flash:  flashFrom(q, t.ID, "", "", ""),
			Error:  q.Get("err"),
			Asset:  h.assetVer,
			Seq:    seq,
			Here:   here(r),
			Theme:  theme(r),
			Wink:   winks(),
		},
		Item: newTodoView(t, true, "", ""),
		Do:   mode(q.Get("do"), t.State),
	}
	if edit != nil {
		data.Edit, data.Error = edit, edit.Error
	}
	h.render(w, "todo.html", data)
}

// back redirects to where the action came from — the item's own page when
// back_id is set, otherwise the list view, with back_open's entry in the
// drawer — carrying either what was done (see flashFrom) or the store's
// validation message, so the page can say so. A failed form comes back
// still open (back_do, back_new), so the message lands beside it.
func back(w http.ResponseWriter, r *http.Request, err error, did url.Values) {
	http.Redirect(w, r, backURL(r, err, did), http.StatusSeeOther)
}

// backURL is where back sends the answer to r.
func backURL(r *http.Request, err error, did url.Values) string {
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
	return dest
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
	var undo string
	if err == nil {
		var t store.Todo
		t, undo, err = h.store.CloseTodo(id, outcome, r.FormValue("reason"))
		outcome = t.State
	}
	back(w, r, err, did("closed", id, "outcome", outcome, "undo", undo))
}

func (h *Handler) reopen(w http.ResponseWriter, r *http.Request) {
	id, err := formID(r)
	if err == nil {
		open := "open"
		_, err = h.store.UpdateTodo(id, store.TodoUpdate{State: &open})
	}
	back(w, r, err, did("reopened", id))
}

// undo reverses the close its toast reported, and no later one: the entry
// comes back exactly as it was, in the drawer or on the page it was closed
// from.
func (h *Handler) undo(w http.ResponseWriter, r *http.Request) {
	id, err := formID(r)
	if err == nil {
		_, err = h.store.UndoClose(id, r.FormValue("undo"))
	}
	d := did("restored", id)
	if r.FormValue("back_id") == "" {
		d.Set("open", strconv.FormatInt(id, 10))
	}
	back(w, r, err, d)
}

// update changes only the fields the form posts. The title edited in place
// posts a title alone, and must leave the body and scope as they are now,
// not as the page last saw them: an agent may have changed them since. An
// edit form posts the Rev it was rendered at as base, the title's field the
// TitleRev as base_title, and either is refused when the entry has moved on
// (see refused).
func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id, err := formID(r)
	title, body, scope := posted(r, "title"), posted(r, "body"), posted(r, "scope")
	if err == nil {
		_, err = h.store.UpdateTodo(id, store.TodoUpdate{
			Title: title, Body: body, Scope: scope,
			IfRev: r.PostForm.Get("base"), IfTitleRev: r.PostForm.Get("base_title"),
		})
	}
	var changed store.ChangedError
	if errors.As(err, &changed) {
		h.refused(w, r, id, changed)
		return
	}
	back(w, r, err, did("saved", id))
}

// refused answers a save the entry moved past with the page the form came
// from, not a redirect to it: the edit form, holding the text just posted
// merged into the entry as it is now, and the entry's new base (see
// newEditView). A redirect would have only the entry to show, and without
// script or storage the typed text would be gone.
//
// The page is named by the posted id alone, never by back_open or back_id, so
// no post can put one entry's text in another entry's form; back_id only says
// the form was on the entry's own page, and the list's filters come along.
func (h *Handler) refused(w http.ResponseWriter, r *http.Request, id int64, why error) {
	cur, err := h.store.GetTodo(id)
	if err != nil {
		back(w, r, err, nil)
		return
	}
	q := url.Values{"do": {"edit"}}
	dest := fmt.Sprintf("/todo/%d", id)
	if r.FormValue("back_id") == "" {
		dest = "/"
		q.Set("open", strconv.FormatInt(id, 10))
		for _, k := range []string{"state", "scope", "q"} {
			if s := r.FormValue("back_" + k); s != "" && !(k == "state" && s == "open") {
				q.Set(k, s)
			}
		}
	}
	dest += "?" + q.Encode()
	page := r.Clone(r.Context())
	page.Method, page.RequestURI = http.MethodGet, dest
	if page.URL, err = url.Parse(dest); err != nil {
		back(w, r, err, nil)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Push-Url", here(page))
	}
	edit := newEditView(cur, r, why.Error())
	if strings.HasPrefix(page.URL.Path, "/todo/") {
		page.SetPathValue("id", strconv.FormatInt(id, 10))
		h.itemPage(w, page, edit)
		return
	}
	h.indexPage(w, page, edit)
}

// posted is a form field's value, or nil when the form has no such field.
func posted(r *http.Request, key string) *string {
	if vs, ok := r.PostForm[key]; ok && len(vs) > 0 {
		return &vs[0]
	}
	return nil
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// uploadImage takes one image from the body editor's script and answers with
// the marker to put in the body.
func (h *Handler) uploadImage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, store.MaxImageBytes+64<<10)
	f, _, err := r.FormFile("image")
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeJSON(w, http.StatusRequestEntityTooLarge,
				map[string]string{"error": fmt.Sprintf("image exceeds %d MB", store.MaxImageBytes>>20)})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no image in the upload"})
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "upload interrupted"})
		return
	}
	id, err := h.store.AddImage(data)
	var ve store.ValidationError
	switch {
	case errors.As(err, &ve):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": ve.Error()})
		return
	case err != nil:
		log.Printf("web: store image: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage failure"})
		return
	}
	src := fmt.Sprintf("/image/%d", id)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "url": src, "markdown": "![image](" + src + ")"})
}

func (h *Handler) image(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	mime, data, err := h.store.GetImage(id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// An id is never given other bytes, so the browser can keep it for good.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

func cookie(r *http.Request, name string) string {
	if c, err := r.Cookie(name); err == nil {
		return c.Value
	}
	return ""
}

// Cookies set before mroc 3.0.0 still say paper or ink, and live a year.
func theme(r *http.Request) string {
	switch cookie(r, themeCookie) {
	case "light", "paper":
		return "light"
	case "dark", "ink":
		return "dark"
	}
	return ""
}

// pref stores one view preference and returns to the page it was set from.
// A value outside allowed clears it, back to the default. Only a local path
// is followed back, so the form cannot send someone off-site.
func pref(name string, allowed ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := &http.Cookie{Name: name, Path: "/", HttpOnly: true, Secure: link.HTTPS(r),
			SameSite: http.SameSiteLaxMode, MaxAge: -1}
		for _, v := range allowed {
			if r.FormValue("to") == v {
				c.Value, c.MaxAge = v, 365*24*60*60
			}
		}
		http.SetCookie(w, c)
		dest := r.FormValue("back")
		if !strings.HasPrefix(dest, "/") || strings.HasPrefix(dest, "//") || strings.HasPrefix(dest, "/\\") {
			dest = "/"
		}
		http.Redirect(w, r, dest, http.StatusSeeOther)
	}
}

// reportKeys are the query keys through which back reports what an action
// did, for flashFrom, or why it failed. They describe a moment, not a view.
var reportKeys = []string{"did", "id", "outcome", "undo", "duplicate", "err"}

// here is the URL a page stands for: the request's, without reportKeys.
// app.js puts it in the address bar once the page shows, so a reload, a
// bookmark or a view switch never repeats an old toast or error.
func here(r *http.Request) string {
	q := r.URL.Query()
	reported := false
	for _, k := range reportKeys {
		reported = reported || q.Has(k)
		q.Del(k)
	}
	switch {
	case !reported:
		return r.URL.RequestURI()
	case len(q) == 0:
		return r.URL.EscapedPath()
	}
	return r.URL.EscapedPath() + "?" + q.Encode()
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
