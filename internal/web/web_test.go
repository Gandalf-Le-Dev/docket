package web

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gandalf-Le-Dev/docket/internal/store"
)

func newEnv(t *testing.T) (*httptest.Server, *store.Store, string, string) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	review, err := s.CreateToken("phone", "review")
	if err != nil {
		t.Fatal(err)
	}
	publish, err := s.CreateToken("box-1", "publish")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHandler(s, ""))
	t.Cleanup(srv.Close)
	return srv, s, review, publish
}

func client(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

func TestIndexRequiresLogin(t *testing.T) {
	srv, _, _, _ := newEnv(t)
	c := client(t)
	resp, err := c.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Request.URL.Path != "/login" {
		t.Fatalf("unauthenticated / landed on %s", resp.Request.URL.Path)
	}
}

func TestPublishTokenRejected(t *testing.T) {
	srv, _, _, publish := newEnv(t)
	c := client(t)
	resp, err := c.PostForm(srv.URL+"/login", url.Values{"token": {publish}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Request.URL.Path != "/login" {
		t.Fatalf("publish token accepted: landed on %s", resp.Request.URL.Path)
	}
}

func TestLoginAddCloseFlow(t *testing.T) {
	srv, s, review, _ := newEnv(t)
	c := client(t)

	resp, err := c.PostForm(srv.URL+"/login", url.Values{"token": {review}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Request.URL.Path != "/" {
		t.Fatalf("login landed on %s", resp.Request.URL.Path)
	}

	resp, err = c.PostForm(srv.URL+"/add", url.Values{
		"title": {"from the web"},
		"body":  {"typed on a phone"},
		"scope": {"Personal"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if !strings.Contains(body, "from the web") || !strings.Contains(body, "personal") {
		t.Fatalf("index missing new item:\n%s", body)
	}

	todos, err := s.ListTodos("open", "", "")
	if err != nil || len(todos) != 1 {
		t.Fatalf("store after add: %v %v", todos, err)
	}
	if todos[0].Via != "phone" || todos[0].Source != "web" {
		t.Fatalf("web add attribution: %+v", todos[0])
	}

	resp, err = c.PostForm(srv.URL+"/todo/close", url.Values{
		"id":      {"1"},
		"outcome": {"done"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got, _ := s.GetTodo(1)
	if got.State != "done" {
		t.Fatalf("close via web: %+v", got)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

func login(t *testing.T, c *http.Client, srv *httptest.Server, token string) {
	t.Helper()
	resp, err := c.PostForm(srv.URL+"/login", url.Values{"token": {token}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestItemPage(t *testing.T) {
	srv, s, review, _ := newEnv(t)
	c := client(t)

	// The page is gated like the rest.
	resp, err := c.Get(srv.URL + "/todo/1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Request.URL.Path != "/login" {
		t.Fatalf("unauthenticated item page landed on %s", resp.Request.URL.Path)
	}

	login(t, c, srv, review)
	id, _, err := s.AddTodo("linkable", "see https://github.com/x/y/pull/7, then (https://example.com/a).", "docket", "test", "phone")
	if err != nil {
		t.Fatal(err)
	}

	resp, err = c.Get(srv.URL + "/todo/1")
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "linkable") {
		t.Fatalf("item page: %d\n%s", resp.StatusCode, body)
	}
	for _, want := range []string{
		`<details class="todo" open>`,
		`<a href="https://github.com/x/y/pull/7" rel="noopener">https://github.com/x/y/pull/7</a>,`,
		`(<a href="https://example.com/a" rel="noopener">https://example.com/a</a>).`,
		`Link: <a href="` + srv.URL + `/todo/1">`,
		`name="back_id" value="1"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("item page missing %q:\n%s", want, body)
		}
	}

	// Actions taken on the page return to it.
	resp, err = c.PostForm(srv.URL+"/todo/close", url.Values{"id": {"1"}, "outcome": {"done"}, "back_id": {"1"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Request.URL.Path != "/todo/1" {
		t.Fatalf("close from item page landed on %s", resp.Request.URL.Path)
	}
	if got, _ := s.GetTodo(id); got.State != "done" {
		t.Fatalf("close from item page: %+v", got)
	}

	// The list links each item to its page.
	resp, err = c.Get(srv.URL + "/?state=done")
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, resp); !strings.Contains(body, `<a class="id" href="/todo/1">#1</a>`) {
		t.Fatalf("list missing permalink:\n%s", body)
	}

	for _, path := range []string{"/todo/999", "/todo/abc"} {
		resp, err = c.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
	}
}

func TestLinkify(t *testing.T) {
	cases := map[string]string{
		"plain <b>text</b>":                       "plain &lt;b&gt;text&lt;/b&gt;",
		"https://a.example/p?x=1&y=2":             `<a href="https://a.example/p?x=1&amp;y=2" rel="noopener">https://a.example/p?x=1&amp;y=2</a>`,
		"end. https://a.example/q.":               `end. <a href="https://a.example/q" rel="noopener">https://a.example/q</a>.`,
		"https://en.wikipedia.org/wiki/Go_(game)": `<a href="https://en.wikipedia.org/wiki/Go_(game)" rel="noopener">https://en.wikipedia.org/wiki/Go_(game)</a>`,
		"'https://a.example/x'":                   `&#39;<a href="https://a.example/x" rel="noopener">https://a.example/x</a>&#39;`,
	}
	for in, want := range cases {
		if got := string(linkify(in)); got != want {
			t.Errorf("linkify(%q)\n got %s\nwant %s", in, got, want)
		}
	}
}

func TestActionsReportBack(t *testing.T) {
	srv, s, review, _ := newEnv(t)
	c := client(t)
	login(t, c, srv, review)

	resp, err := c.PostForm(srv.URL+"/add", url.Values{"title": {"first"}, "scope": {"docket"}})
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if !strings.Contains(body, "Filed #1.") || !strings.Contains(body, `<a href="/todo/1">Open it</a>`) {
		t.Fatalf("add flash missing:\n%s", body)
	}
	// Scope inputs complete from the scopes in use.
	if !strings.Contains(body, `<datalist id="scopes"><option value="docket"></datalist>`) {
		t.Fatalf("scope datalist missing:\n%s", body)
	}

	resp, err = c.PostForm(srv.URL+"/add", url.Values{"title": {"first"}, "scope": {"docket"}})
	if err != nil {
		t.Fatal(err)
	}
	if body = readAll(t, resp); !strings.Contains(body, "Already open as #1.") {
		t.Fatalf("duplicate flash missing:\n%s", body)
	}

	resp, err = c.PostForm(srv.URL+"/todo/close", url.Values{"id": {"1"}, "outcome": {"dropped"}, "reason": {"not now"}, "back_state": {"open"}})
	if err != nil {
		t.Fatal(err)
	}
	body = readAll(t, resp)
	if !strings.Contains(body, "Dropped #1.") || !strings.Contains(body, `action="/todo/reopen"`) {
		t.Fatalf("drop flash missing its reverse:\n%s", body)
	}

	// The reverse lands where the action came from, and says so.
	resp, err = c.PostForm(srv.URL+"/todo/reopen", url.Values{"id": {"1"}, "back_state": {"dropped"}})
	if err != nil {
		t.Fatal(err)
	}
	body = readAll(t, resp)
	if resp.Request.URL.Query().Get("state") != "dropped" || !strings.Contains(body, "Reopened #1.") {
		t.Fatalf("reopen landed on %s:\n%s", resp.Request.URL, body)
	}
	if got, _ := s.GetTodo(1); got.State != "open" {
		t.Fatalf("reopen: %+v", got)
	}

	// A closed item shows its latest verdict on its own line; the earlier
	// drop note stays in the body as history.
	c.PostForm(srv.URL+"/todo/close", url.Values{"id": {"1"}, "outcome": {"done"}, "reason": {"shipped"}})
	resp, err = c.Get(srv.URL + "/todo/1")
	if err != nil {
		t.Fatal(err)
	}
	body = readAll(t, resp)
	if !strings.Contains(body, `<p class="verdict"><b>Done:</b> shipped</p>`) ||
		!strings.Contains(body, `<div class="body">— closed (dropped): not now</div>`) {
		t.Fatalf("verdict rendering:\n%s", body)
	}
}

func TestSplitVerdict(t *testing.T) {
	cases := []struct{ body, text, verdict string }{
		{"", "", ""},
		{"plain body", "plain body", ""},
		{"— closed (done): shipped", "", "shipped"},
		{"context\n\n— closed (dropped): not now", "context", "not now"},
		{"mentions — closed (x): inline, not a note", "mentions — closed (x): inline, not a note", ""},
	}
	for _, c := range cases {
		text, verdict := splitVerdict(c.body)
		if text != c.text || verdict != c.verdict {
			t.Errorf("splitVerdict(%q) = %q, %q; want %q, %q", c.body, text, verdict, c.text, c.verdict)
		}
	}
}

func TestAgo(t *testing.T) {
	stamp := func(d time.Duration) string { return time.Now().Add(-d).UTC().Format(time.RFC3339) }
	cases := map[string]string{
		stamp(10 * time.Second):   "just now",
		stamp(5 * time.Minute):    "5m ago",
		stamp(3 * time.Hour):      "3h ago",
		stamp(4 * 24 * time.Hour): "4d ago",
		"2020-02-03T04:05:06Z":    "Feb 3, 2020",
		"garbage":                 "garbage",
	}
	for in, want := range cases {
		if got := ago(in); got != want {
			t.Errorf("ago(%q) = %q, want %q", in, got, want)
		}
	}
}
