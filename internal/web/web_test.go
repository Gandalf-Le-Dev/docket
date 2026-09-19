package web

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

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
