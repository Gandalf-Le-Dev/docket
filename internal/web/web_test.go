package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
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
	srv := httptest.NewServer(NewHandler(s))
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

// An htmx request without a session loads the login page whole instead of
// swapping it into the page it came from; so does signing out.
func TestHTMXGoesToLoginWhole(t *testing.T) {
	srv, _, review, _ := newEnv(t)
	c := client(t)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	send := func(method, path string, header http.Header) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		req.Header = header.Clone()
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	htmx := http.Header{"Hx-Request": {"true"}}
	for _, r := range []struct{ method, path string }{{"GET", "/?open=1"}, {"POST", "/todo/close"}} {
		resp := send(r.method, r.path, htmx)
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("HX-Redirect") != "/login" {
			t.Fatalf("htmx %s %s: %d, HX-Redirect %q", r.method, r.path, resp.StatusCode, resp.Header.Get("HX-Redirect"))
		}
	}
	// The back button's restore cannot be redirected by header, so it follows
	// the ordinary redirect.
	resp := send("GET", "/", http.Header{"Hx-Request": {"true"}, "Hx-History-Restore-Request": {"true"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("history restore: %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp := send("GET", "/", nil); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("HX-Redirect") != "" {
		t.Fatalf("plain request: %d, HX-Redirect %q", resp.StatusCode, resp.Header.Get("HX-Redirect"))
	}

	login(t, c, srv, review)
	if resp := send("GET", "/", htmx); resp.StatusCode != http.StatusOK {
		t.Fatalf("signed in htmx request: %d", resp.StatusCode)
	}
	resp = send("POST", "/logout", htmx)
	if resp.Header.Get("HX-Redirect") != "/login" {
		t.Fatalf("htmx sign out: %d, HX-Redirect %q", resp.StatusCode, resp.Header.Get("HX-Redirect"))
	}
	if resp := send("GET", "/", htmx); resp.Header.Get("HX-Redirect") != "/login" {
		t.Fatalf("still signed in after sign out: %d", resp.StatusCode)
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
		`<h1 class="display-m">linkable</h1>`,
		`<a href="https://github.com/x/y/pull/7" rel="noopener">https://github.com/x/y/pull/7</a>,`,
		`(<a href="https://example.com/a" rel="noopener">https://example.com/a</a>).`,
		`<dt>From</dt><dd>test</dd>`, // the source, labeled
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

	// The list opens an entry in the drawer, and the drawer links on to its page.
	resp, err = c.Get(srv.URL + "/?state=done&open=1")
	if err != nil {
		t.Fatal(err)
	}
	body = readAll(t, resp)
	for _, want := range []string{
		`href="/?open=1&amp;state=done"`,          // the row opens the drawer
		`<a class="self" href="/todo/1"`,          // the drawer opens the page
		`class="entry on"`,                        // the open row is marked
		`<a class="backdrop" href="/?state=done"`, // a click outside closes it
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("drawer list missing %q:\n%s", want, body)
		}
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

func TestRenderBody(t *testing.T) {
	cases := map[string]string{
		"shot:\n![image](/image/7)\nafter": "shot:\n" +
			`<a href="/image/7" target="_blank" rel="noopener"><img src="/image/7" alt="image" loading="lazy"></a>` + "\nafter",
		`![x"><script>alert(1)</script>](/image/2)`: `<a href="/image/2" target="_blank" rel="noopener">` +
			`<img src="/image/2" alt="x&#34;&gt;&lt;script&gt;alert(1)&lt;/script&gt;" loading="lazy"></a>`,
		// alt text that looks like a URL is not also turned into a link
		"![https://a.example/](/image/3)":               `<a href="/image/3" target="_blank" rel="noopener"><img src="/image/3" alt="https://a.example/" loading="lazy"></a>`,
		"see https://a.example/p.":                      `see <a href="https://a.example/p" rel="noopener">https://a.example/p</a>.`,
		"![a](/image/abc) ![b](//evil.example/image/1)": "![a](/image/abc) ![b](//evil.example/image/1)",
	}
	for in, want := range cases {
		if got := string(renderBody(in)); got != want {
			t.Errorf("renderBody(%q)\n got %s\nwant %s", in, got, want)
		}
	}
	got := string(renderBody("![x](https://evil.example/a.png)"))
	if strings.Contains(got, "<img") || !strings.HasPrefix(got, "![x](") {
		t.Errorf("remote image rendered: %s", got)
	}
	if got := excerpt("before ![shot](/image/1) after"); got != "before [image] after" {
		t.Errorf("excerpt = %q", got)
	}
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func upload(t *testing.T, c *http.Client, srv *httptest.Server, data []byte, header http.Header) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("image", "shot.png")
	fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/image", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestImageUpload(t *testing.T) {
	srv, s, review, _ := newEnv(t)
	c := client(t)
	pic := tinyPNG(t)

	resp := upload(t, c, srv, pic, nil)
	resp.Body.Close()
	if resp.Request.URL.Path != "/login" {
		t.Fatalf("unauthenticated upload landed on %s", resp.Request.URL.Path)
	}

	login(t, c, srv, review)
	resp = upload(t, c, srv, pic, http.Header{"Sec-Fetch-Site": {"same-origin"}})
	var got struct {
		ID       int64  `json:"id"`
		URL      string `json:"url"`
		Markdown string `json:"markdown"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("upload: %d %v", resp.StatusCode, err)
	}
	resp.Body.Close()
	if got.URL != fmt.Sprintf("/image/%d", got.ID) || got.Markdown != "![image]("+got.URL+")" {
		t.Fatalf("upload answer: %+v", got)
	}
	if _, data, err := s.GetImage(got.ID); err != nil || !bytes.Equal(data, pic) {
		t.Fatalf("stored image: %v", err)
	}

	for name, tc := range map[string]struct {
		data   []byte
		header http.Header
		status int
	}{
		"not an image": {[]byte("<svg><script>alert(1)</script></svg>"), nil, http.StatusBadRequest},
		"too big":      {make([]byte, 6<<20), nil, http.StatusRequestEntityTooLarge},
		"cross-origin": {pic, http.Header{"Sec-Fetch-Site": {"same-site"}}, http.StatusForbidden},
	} {
		resp := upload(t, c, srv, tc.data, tc.header)
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&e)
		resp.Body.Close()
		if resp.StatusCode != tc.status || e.Error == "" {
			t.Errorf("%s: %d %q", name, resp.StatusCode, e.Error)
		}
	}
}

// Every post, not only the image upload, refuses another origin: a sibling
// subdomain still carries the SameSite=Lax cookie. A request with neither
// Sec-Fetch-Site nor Origin is not a browser's cross-site one, and passes.
func TestCrossOriginPostsRefused(t *testing.T) {
	srv, s, review, _ := newEnv(t)
	c := client(t)
	login(t, c, srv, review)
	id, _, _ := s.AddTodo("keep me open", "", "", "test", "t")
	post := func(path string, header http.Header) *http.Response {
		t.Helper()
		form := url.Values{"id": {"1"}, "title": {"planted"}, "token": {review}, "to": {"dark"}}.Encode()
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(form))
		req.Header = header.Clone()
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	for _, header := range []http.Header{
		{"Sec-Fetch-Site": {"same-site"}},
		{"Sec-Fetch-Site": {"cross-site"}},
		{"Origin": {"https://sibling.example"}},
	} {
		for _, path := range []string{"/todo/close", "/add", "/logout", "/login", "/theme", "/image"} {
			if resp := post(path, header); resp.StatusCode != http.StatusForbidden {
				t.Fatalf("%v post to %s: %d", header, path, resp.StatusCode)
			}
		}
	}
	if got, _ := s.GetTodo(id); got.State != "open" {
		t.Fatalf("cross-origin close went through: %+v", got)
	}
	if todos, _ := s.ListTodos("all", "", ""); len(todos) != 1 {
		t.Fatalf("cross-origin add went through: %+v", todos)
	}
	for _, header := range []http.Header{{"Sec-Fetch-Site": {"same-origin"}}, {}} {
		open := "open"
		s.UpdateTodo(id, store.TodoUpdate{State: &open})
		resp := post("/todo/close", header)
		if got, _ := s.GetTodo(id); resp.StatusCode != http.StatusOK || got.State != "done" {
			t.Fatalf("%v close: %d %+v", header, resp.StatusCode, got)
		}
	}
}

func TestImageServe(t *testing.T) {
	srv, s, review, _ := newEnv(t)
	c := client(t)
	pic := tinyPNG(t)
	id, err := s.AddImage(pic)
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/image/%d", id)

	resp, err := c.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Request.URL.Path != "/login" {
		t.Fatalf("unauthenticated image landed on %s", resp.Request.URL.Path)
	}

	login(t, c, srv, review)
	resp, err = c.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, resp); body != string(pic) {
		t.Fatalf("image bytes differ: %d vs %d", len(body), len(pic))
	}
	for k, want := range map[string]string{
		"Content-Type":            "image/png",
		"X-Content-Type-Options":  "nosniff",
		"Cache-Control":           "private, max-age=31536000, immutable",
		"Content-Security-Policy": "default-src 'none'",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	for _, p := range []string{"/image/999", "/image/abc"} {
		resp, err := c.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
	}

	// The item page and the drawer draw the body's and the verdict's images;
	// the list row shows a word in the verdict's place.
	tid, _, _ := s.AddTodo("with a shot", "look:\n![image]("+path+")", "", "test", "t")
	s.CloseTodo(tid, "done", "fixed, see ![after]("+path+")")
	var body string
	for _, page := range []string{fmt.Sprintf("/todo/%d", tid), fmt.Sprintf("/?state=done&open=%d", tid)} {
		resp, err = c.Get(srv.URL + page)
		if err != nil {
			t.Fatal(err)
		}
		body = readAll(t, resp)
		for _, alt := range []string{"image", "after"} {
			if !strings.Contains(body, `<img src="`+path+`" alt="`+alt+`" loading="lazy">`) {
				t.Fatalf("%s lacks the %s image:\n%s", page, alt, body)
			}
		}
	}
	if !strings.Contains(body, `<span class="reason">fixed, see [image]</span>`) {
		t.Fatalf("list row shows the marker:\n%s", body)
	}
}

// A body's links, same-site ones included, are plain links: htmx must not
// boost /image/5 or /healthz into a swap that expects a page.
func TestBodiesAreNotBoosted(t *testing.T) {
	srv, s, review, _ := newEnv(t)
	c := client(t)
	login(t, c, srv, review)
	id, _, _ := s.AddTodo("links", "see "+srv.URL+"/healthz", "", "test", "t")
	s.CloseTodo(id, "done", "see "+srv.URL+"/image/5")
	for _, page := range []string{"/todo/1", "/?state=done&open=1"} {
		resp, err := c.Get(srv.URL + page)
		if err != nil {
			t.Fatal(err)
		}
		body := readAll(t, resp)
		for _, want := range []string{
			`<div class="text" hx-boost="false">see <a href="` + srv.URL + `/healthz"`,
			`<p class="verdict" hx-boost="false"><b>Done.</b> see <a href="` + srv.URL + `/image/5"`,
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s missing %q:\n%s", page, want, body)
			}
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
	// Filing lands with what was filed open in the drawer that held the form.
	if !strings.Contains(body, "Filed #1.") || !strings.Contains(body, `aria-label="Entry 1"`) {
		t.Fatalf("add flash or drawer missing:\n%s", body)
	}
	// Scope inputs complete from the scopes in use.
	if !strings.Contains(body, `<option value="docket">`) {
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
	if !strings.Contains(body, "Dropped.") || !strings.Contains(body, `action="/todo/undo"`) {
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
	// The note that matches the current state is the verdict; the earlier drop
	// note stays in the body as history.
	if !strings.Contains(body, `<b>Done.</b> shipped`) ||
		!strings.Contains(body, `— closed (dropped): not now`) {
		t.Fatalf("verdict rendering:\n%s", body)
	}
}

// Done and Drop answer with a toast in the live region, whose Undo puts the
// entry back exactly and lands where the close came from.
func TestUndoFromToast(t *testing.T) {
	srv, s, review, _ := newEnv(t)
	c := client(t)
	login(t, c, srv, review)
	id, _, _ := s.AddTodo("undo me", "why\n\n— closed (done): an older verdict", "pilot", "test", "t")
	before, _ := s.GetTodo(id)
	post := func(path string, form url.Values) (*http.Response, string) {
		t.Helper()
		resp, err := c.PostForm(srv.URL+path, form)
		if err != nil {
			t.Fatal(err)
		}
		return resp, readAll(t, resp)
	}
	// toastUndo is the form the toast offers, as a browser would submit it.
	toastUndo := func(body string) url.Values {
		t.Helper()
		toast := body[strings.Index(body, `<div id="toasts" class="toasts" role="status">`):]
		form := url.Values{}
		for _, m := range regexp.MustCompile(`name="(\w+)" value="([^"]*)"`).FindAllStringSubmatch(toast, -1) {
			form.Set(m[1], html.UnescapeString(m[2]))
		}
		if form.Get("undo") == "" {
			t.Fatalf("toast has no undo token:\n%s", toast)
		}
		return form
	}

	_, body := post("/todo/close", url.Values{
		"id": {"1"}, "outcome": {"dropped"}, "reason": {"not now"},
		"back_state": {"open"}, "back_scope": {"pilot"}, "back_q": {"undo"},
	})
	toast := body[strings.Index(body, `<div id="toasts" class="toasts" role="status">`):]
	for _, want := range []string{
		`<div hx-swap-oob="innerHTML:#toasts"><div class="toast">`,
		`<span>Dropped.</span>`,
		`action="/todo/undo"`,
		`name="back_scope" value="pilot"`,
		`name="back_q" value="undo"`,
		`>Undo</button>`,
	} {
		if !strings.Contains(toast, want) {
			t.Fatalf("toast missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `class="entry`) {
		t.Fatalf("dropped entry still listed:\n%s", body)
	}

	resp, body := post("/todo/undo", toastUndo(body))
	if q := resp.Request.URL.Query(); q.Get("open") != "1" || q.Get("scope") != "pilot" || q.Get("q") != "undo" ||
		!strings.Contains(body, "Restored #1.") || !strings.Contains(body, `class="entry on"`) {
		t.Fatalf("undo landed on %s:\n%s", resp.Request.URL, body)
	}
	if got, _ := s.GetTodo(id); got != before {
		t.Fatalf("undo is not exact:\n got  %+v\n want %+v", got, before)
	}

	// A toast undoes its own close only: once an agent has closed the entry
	// again, the web's Undo is refused and the agent's verdict stands.
	_, body = post("/todo/close", url.Values{"id": {"1"}, "outcome": {"dropped"}, "reason": {"from the web"}})
	first := toastUndo(body)
	agent, _, err := s.CloseTodo(id, "done", "an agent finished it")
	if err != nil {
		t.Fatal(err)
	}
	if _, body = post("/todo/undo", first); !strings.Contains(body, "closed again since") {
		t.Fatalf("stale undo not refused:\n%s", body)
	}
	// The refusal empties the toast region, rather than leave an Undo that can
	// only fail again.
	if toast := body[strings.Index(body, `<div id="toasts"`):]; !strings.HasPrefix(toast,
		`<div id="toasts" class="toasts" role="status"><div hx-swap-oob="innerHTML:#toasts"></div></div>`) {
		t.Fatalf("refused undo leaves its toast:\n%s", toast)
	}
	if got, _ := s.GetTodo(id); got != agent {
		t.Fatalf("stale undo changed the entry: %+v", got)
	}

	// Closed on its own page, the entry comes back there; a second undo has
	// nothing left to reverse and says so.
	_, body = post("/todo/close", url.Values{"id": {"1"}, "back_id": {"1"}})
	form := toastUndo(body)
	for i, want := range []string{"Restored #1.", "changed since it was closed"} {
		resp, body = post("/todo/undo", form)
		if resp.Request.URL.Path != "/todo/1" || !strings.Contains(body, want) {
			t.Fatalf("undo %d landed on %s without %q:\n%s", i+1, resp.Request.URL, want, body)
		}
	}
}

func TestSplitVerdict(t *testing.T) {
	cases := []struct{ body, text, outcome, reason string }{
		{"", "", "", ""},
		{"plain body", "plain body", "", ""},
		{"— closed (done): shipped", "", "done", "shipped"},
		{"context\n\n— closed (dropped): not now", "context", "dropped", "not now"},
		{"mentions — closed (x): inline, not a note", "mentions — closed (x): inline, not a note", "", ""},
		// two closes: the last one is the live verdict
		{"why\n\n— closed (dropped): no\n\n— closed (done): yes", "why\n\n— closed (dropped): no", "done", "yes"},
	}
	for _, c := range cases {
		text, outcome, reason := splitVerdict(c.body)
		if text != c.text || outcome != c.outcome || reason != c.reason {
			t.Errorf("splitVerdict(%q) = %q, %q, %q; want %q, %q, %q",
				c.body, text, outcome, reason, c.text, c.outcome, c.reason)
		}
	}
}

// A note left by an earlier close must not be shown as the verdict for the
// state the item is in now.
func TestVerdictMatchesState(t *testing.T) {
	body := "why\n\n— closed (dropped): superseded"
	v := newTodoView(store.Todo{ID: 1, Body: body, State: "done"}, false, "", "")
	if v.Verdict != "" || v.Text != body {
		t.Fatalf("stale note surfaced: verdict=%q text=%q", v.Verdict, v.Text)
	}
	v = newTodoView(store.Todo{ID: 1, Body: body, State: "dropped"}, false, "", "")
	if v.Verdict != "superseded" || v.Text != "why" {
		t.Fatalf("matching note not used: verdict=%q text=%q", v.Verdict, v.Text)
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

// A busy scope shows every entry: no cap, no "more" link.
func TestPanelShowsEverything(t *testing.T) {
	srv, s, review, _ := newEnv(t)
	c := client(t)
	login(t, c, srv, review)
	const n = 12
	for i := 0; i < n; i++ {
		if _, _, err := s.AddTodo(fmt.Sprintf("entry %d", i), "", "pilot", "test", "t"); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := c.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if got := strings.Count(body, `class="entry"`); got != n || strings.Contains(body, "more in") {
		t.Fatalf("home showed %d of %d entries:\n%s", got, n, body)
	}
}

// Every form opens in place, from a URL: the new entry in the drawer, an
// entry's edit and drop in the drawer or on its page, a scope's rename in its
// panel's header.
func TestFormsOpenInPlace(t *testing.T) {
	srv, s, review, _ := newEnv(t)
	c := client(t)
	login(t, c, srv, review)
	if _, _, err := s.AddTodo("first", "", "pilot", "test", "t"); err != nil {
		t.Fatal(err)
	}
	get := func(path string) string {
		t.Helper()
		resp, err := c.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		return readAll(t, resp)
	}
	for path, wants := range map[string][]string{
		"/?new=1&in=pilot": {`aria-label="New entry"`, `action="/add"`, `name="scope" list="scopes" value="pilot"`, `class="attach" data-for="n-body"`},
		"/?open=1&do=edit": {`id="drawer-edit"`, `name="back_open" value="1"`, `form="drawer-edit"`, `class="attach" data-for="d-body"`},
		"/?open=1&do=drop": {`name="outcome" value="dropped"`, `placeholder="Why not? Kept in the Dropped tab"`},
		"/todo/1?do=edit":  {`action="/todo/update"`, `id="e-title" name="title" value="first"`, `class="attach" data-for="e-body"`},
		"/todo/1?do=drop":  {`name="outcome" value="dropped"`, `name="back_id" value="1"`},
		"/?rename=pilot":   {`action="/scope/rename"`, `name="from" value="pilot"`, `class="name-input"`},
	} {
		body := get(path)
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Fatalf("%s missing %q:\n%s", path, want, body)
			}
		}
	}
	// The scripts are fingerprinted like the stylesheet, so a deploy busts all
	// three, and htmx is there before the script that listens for its events.
	v := hashAsset(fingerprinted...)
	if body := get("/"); !strings.Contains(body, `<script src="/static/htmx.min.js?v=`+v+`" defer></script>
<script src="/static/app.js?v=`+v+`" defer>`) {
		t.Fatalf("page does not load the fingerprinted scripts:\n%s", body)
	}
	// The entry page carries the list's header and tabs, not a lesser one.
	if body := get("/todo/1"); !strings.Contains(body, `class="nav"`) || !strings.Contains(body, `role="search"`) {
		t.Fatalf("entry page lacks the list's chrome:\n%s", body)
	}

	// Saving from the drawer keeps the entry open there.
	resp, err := c.PostForm(srv.URL+"/todo/update", url.Values{
		"id": {"1"}, "title": {"first, edited"}, "scope": {"pilot"}, "back_open": {"1"}, "back_do": {"edit"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.Request.URL.Query().Get("open") != "1" || resp.Request.URL.Query().Get("do") != "" ||
		!strings.Contains(body, "Saved #1.") {
		t.Fatalf("drawer save landed on %s", resp.Request.URL)
	}

	// Renaming the scope the list is filtered to follows it to the new name.
	resp, err = c.PostForm(srv.URL+"/scope/rename", url.Values{"from": {"pilot"}, "to": {"Fleet"}, "back_scope": {"pilot"}})
	if err != nil {
		t.Fatal(err)
	}
	body = readAll(t, resp)
	if resp.Request.URL.Query().Get("scope") != "fleet" || !strings.Contains(body, "first, edited") {
		t.Fatalf("rename landed on %s:\n%s", resp.Request.URL, body)
	}
}

// Links and forms swap #page in place, and each page has one to swap; the
// ones that act beside the list keep its scroll. The two forms that must load
// a whole page are never boosted.
func TestPagesSwapInPlace(t *testing.T) {
	srv, s, review, _ := newEnv(t)
	c := client(t)
	resp, err := c.Get(srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, resp); !strings.Contains(body, `action="/login" hx-boost="false"`) {
		t.Fatalf("login form boosted:\n%s", body)
	}
	login(t, c, srv, review)
	if _, _, err := s.AddTodo("first", "", "pilot", "test", "t"); err != nil {
		t.Fatal(err)
	}
	keep := `hx-swap="outerHTML show:none"`
	// app.js compares it with what it was loaded with, to reload after a deploy.
	page := `<div id="page" data-asset="` + hashAsset(fingerprinted...) + `"`
	for path, wants := range map[string][]string{
		"/":        {page, `<section class="panel" ` + keep, `action="/theme" aria-label="Theme" hx-boost="false"`},
		"/?open=1": {`<aside class="drawer" aria-label="Entry 1" ` + keep, `aria-hidden="true"` + "\n  " + keep},
		"/?new=1":  {`<aside class="drawer" aria-label="New entry" ` + keep},
		"/todo/1":  {page},
	} {
		resp, err := c.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body := readAll(t, resp)
		wants = append(wants, `<body hx-boost="true" hx-target="#page" hx-select="#page" hx-swap="outerHTML">`)
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Fatalf("%s missing %q:\n%s", path, want, body)
			}
		}
	}

	// A failed save comes back to the drawer, and says why there, where the
	// list's scroll cannot hide it.
	resp, err = c.PostForm(srv.URL+"/todo/update", url.Values{
		"id": {"1"}, "title": {" "}, "back_open": {"1"}, "back_do": {"edit"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	drawer := body[strings.Index(body, `<aside class="drawer"`):]
	if strings.Count(body, `class="error"`) != 1 || !strings.Contains(drawer, `<div class="error">title must not be empty</div>`) {
		t.Fatalf("failed save's message not in the drawer:\n%s", body)
	}
}

// A scope with nothing open does not exist on the Open tab: no panel, no
// suggestion. Its closed entries still group under it on the Done tab.
func TestScopeWithNothingOpenIsGone(t *testing.T) {
	srv, s, review, _ := newEnv(t)
	c := client(t)
	login(t, c, srv, review)
	id, _, err := s.AddTodo("finished", "", "personal", "test", "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CloseTodo(id, "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddTodo("live", "", "pilot", "test", "t"); err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if strings.Contains(body, "personal") || !strings.Contains(body, `<option value="pilot">`) {
		t.Fatalf("open tab shows a scope with nothing open:\n%s", body)
	}
	resp, err = c.Get(srv.URL + "/?state=done")
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, resp); !strings.Contains(body, `>personal</a>`) || strings.Contains(body, `>pilot</a>`) {
		t.Fatalf("done tab grouping:\n%s", body)
	}
}

// Scopes take colors in the order they came into use, so the first eight
// never share one and a newer scope never repaints an older one.
func TestScopeHues(t *testing.T) {
	m := scopeHues([]store.ScopeStats{
		{Scope: "b", First: 2}, {Scope: "", First: 1}, {Scope: "a", First: 9}, {Scope: "c", First: 3},
	})
	want := map[string]string{"b": "hue-0", "c": "hue-1", "a": "hue-2"}
	if len(m) != len(want) {
		t.Fatalf("scopeHues = %v", m)
	}
	for k, v := range want {
		if m[k] != v {
			t.Fatalf("scopeHues = %v, want %v", m, want)
		}
	}
}

// The density and theme switches set a cookie, return to the page they were
// used on, and never follow a return path off the site.
func TestViewSwitches(t *testing.T) {
	srv, s, review, _ := newEnv(t)
	c := client(t)
	login(t, c, srv, review)
	if _, _, err := s.AddTodo("live", "", "pilot", "test", "t"); err != nil {
		t.Fatal(err)
	}
	post := func(path, to, back string) (*http.Response, string) {
		t.Helper()
		resp, err := c.PostForm(srv.URL+path, url.Values{"to": {to}, "back": {back}})
		if err != nil {
			t.Fatal(err)
		}
		return resp, readAll(t, resp)
	}

	resp, body := post("/density", "compact", "/?scope=pilot")
	if resp.Request.URL.RequestURI() != "/?scope=pilot" || !strings.Contains(body, `class="with-drawer compact">`) ||
		!strings.Contains(body, `<a class="name tint hue-0"`) {
		t.Fatalf("compact landed on %s:\n%s", resp.Request.URL, body)
	}
	resp, body = post("/theme", "dark", "/todo/1")
	if resp.Request.URL.Path != "/todo/1" || !strings.Contains(body, `<html lang="en" data-theme="dark">`) ||
		!strings.Contains(body, `aria-label="Theme: Dark"`) {
		t.Fatalf("theme landed on %s:\n%s", resp.Request.URL, body)
	}
	if !strings.Contains(body, `class=" tint hue-`) {
		t.Fatalf("entry page lacks its scope color:\n%s", body)
	}
	_, body = post("/theme", "auto", "/")
	if strings.Contains(body, "data-theme") || !strings.Contains(body, `value="auto" class="on"`) ||
		!strings.Contains(body, `aria-label="Theme: System"`) {
		t.Fatalf("auto did not clear the theme:\n%s", body)
	}
	for _, back := range []string{"https://evil.example/", "//evil.example/", `/\evil.example/`} {
		resp, _ = post("/density", "detailed", back)
		if resp.Request.URL.Host != strings.TrimPrefix(srv.URL, "http://") || resp.Request.URL.Path != "/" {
			t.Fatalf("back %q followed to %s", back, resp.Request.URL)
		}
	}
}

// A theme cookie from before the light/dark rename keeps its choice, and an
// unknown one falls back to following the OS.
func TestThemeCookie(t *testing.T) {
	for v, want := range map[string]string{
		"light": "light", "dark": "dark", "paper": "light", "ink": "dark", "sepia": "", "": "",
	} {
		r := httptest.NewRequest("GET", "/", nil)
		if v != "" {
			r.AddCookie(&http.Cookie{Name: themeCookie, Value: v})
		}
		if got := theme(r); got != want {
			t.Errorf("theme(%q) = %q, want %q", v, got, want)
		}
	}
}

// Each page load rolls three winks, in order, the first within 8 to 40 seconds.
func TestWinks(t *testing.T) {
	for i := 0; i < 200; i++ {
		var w1, w2, w3 float64
		if _, err := fmt.Sscanf(string(winks()), "--w1: %fs; --w2: %fs; --w3: %fs", &w1, &w2, &w3); err != nil {
			t.Fatalf("winks() = %q: %v", winks(), err)
		}
		if w1 < 8 || w1 > 40 || w2 < w1+30 || w3 < w2+60 {
			t.Fatalf("winks out of range: %v %v %v", w1, w2, w3)
		}
	}
}
