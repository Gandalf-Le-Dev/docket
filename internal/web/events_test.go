package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Gandalf-Le-Dev/docket/internal/store"
)

// stream is an open /events response, read a line at a time.
type stream struct {
	resp  *http.Response
	lines chan string
	stop  context.CancelFunc
}

func openStream(t *testing.T, c *http.Client, srv *httptest.Server) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/events", nil)
	resp, err := c.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	s := &stream{resp: resp, lines: make(chan string, 64), stop: cancel}
	go func() {
		defer close(s.lines)
		r := bufio.NewReader(resp.Body)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			s.lines <- strings.TrimRight(line, "\n")
		}
	}()
	t.Cleanup(func() {
		cancel()
		resp.Body.Close()
	})
	return s
}

// until reads lines until one starts with prefix.
func (s *stream) until(t *testing.T, prefix string) string {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case line, ok := <-s.lines:
			if !ok {
				t.Fatalf("stream ended before a line starting %q", prefix)
			}
			if strings.HasPrefix(line, prefix) {
				return line
			}
		case <-deadline:
			t.Fatalf("no line starting %q", prefix)
		}
	}
}

func eventsEnv(t *testing.T, beat time.Duration) (*httptest.Server, *store.Store, *http.Client) {
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
	h := NewHandler(s)
	h.beat = beat
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := client(t)
	login(t, c, srv, review)
	return srv, s, c
}

// Without a session the stream answers 204, which tells EventSource to stop
// reconnecting, rather than redirecting it to a login page it cannot show.
func TestEventsSignedOut(t *testing.T) {
	srv, s, _, publish := newEnv(t)
	c := client(t)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for _, token := range []string{"", "garbage", publish} {
		req, _ := http.NewRequest("GET", srv.URL+"/events", nil)
		if token != "" {
			req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("token %q: %d, want 204", token, resp.StatusCode)
		}
	}
	if n := s.Subscribers(); n != 0 {
		t.Fatalf("%d subscribers left behind", n)
	}
}

func TestEventsStreamChanges(t *testing.T) {
	srv, s, c := eventsEnv(t, time.Hour)
	st := openStream(t, c, srv)
	if st.resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", st.resp.StatusCode)
	}
	if ct := st.resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type %q", ct)
	}
	if cc := st.resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control %q", cc)
	}

	st.until(t, "event: seq")
	st.until(t, "data: ")
	id, _, err := s.AddTodo("secret title", "secret body", "", "", "x")
	if err != nil {
		t.Fatal(err)
	}
	line := st.until(t, "data: ")
	var got store.Change
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &got); err != nil {
		t.Fatalf("%q: %v", line, err)
	}
	if got.ID != id || got.Op != store.Added || got.Seq != s.Seq() {
		t.Fatalf("event %+v", got)
	}
	if strings.Contains(line, "secret") {
		t.Fatalf("event carries the item's text: %q", line)
	}
}

func TestEventsHeartbeat(t *testing.T) {
	srv, _, c := eventsEnv(t, 10*time.Millisecond)
	st := openStream(t, c, srv)
	st.until(t, ":")
	st.until(t, ":")
}

// A stream whose session is revoked ends at the next heartbeat.
func TestEventsEndWithTheSession(t *testing.T) {
	srv, s, c := eventsEnv(t, 10*time.Millisecond)
	st := openStream(t, c, srv)
	st.until(t, ":")
	if err := s.RevokeToken("phone"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-st.lines:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("stream outlived its session")
		}
	}
}

func TestEventsEndWhenClientLeaves(t *testing.T) {
	srv, s, c := eventsEnv(t, time.Hour)
	st := openStream(t, c, srv)
	waitSubscribers(t, s, 1)
	st.stop()
	waitSubscribers(t, s, 0)
}

func waitSubscribers(t *testing.T, s *store.Store, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for s.Subscribers() != want {
		if time.Now().After(deadline) {
			t.Fatalf("%d subscribers, want %d", s.Subscribers(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var seqAttr = regexp.MustCompile(`id="page"[^>]* data-seq="([^"]+)"`)

func pageSeq(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	m := seqAttr.FindStringSubmatch(readAll(t, resp))
	if m == nil {
		t.Fatalf("%s: #page without data-seq", url)
	}
	return m[1]
}

// A page carries the Seq it was rendered at and the stream opens with the
// current one, so a page that missed a change between its render and its
// stream opening sees the two differ.
func TestSeqTellsAPageItIsBehind(t *testing.T) {
	srv, s, c := eventsEnv(t, time.Hour)
	s.AddTodo("item", "", "", "", "x")
	for _, path := range []string{"/", "/todo/1"} {
		if got := pageSeq(t, c, srv.URL+path); got != s.Seq() {
			t.Fatalf("%s: data-seq %q, store %q", path, got, s.Seq())
		}
	}
	rendered := pageSeq(t, c, srv.URL+"/")

	st := openStream(t, c, srv)
	if line := st.until(t, "event: "); line != "event: seq" {
		t.Fatalf("first event %q", line)
	}
	if line := st.until(t, "data: "); line != "data: "+rendered {
		t.Fatalf("first data %q, want the rendered %q", line, rendered)
	}

	s.AddTodo("between render and connect", "", "", "", "x")
	st2 := openStream(t, c, srv)
	st2.until(t, "event: seq")
	if line := st2.until(t, "data: "); line == "data: "+rendered {
		t.Fatalf("seq did not move past the render: %q", line)
	}
}

// A row names its item and version, so a live refresh can tell which rows
// are new or changed.
func TestRowsCarryRevision(t *testing.T) {
	srv, s, c := eventsEnv(t, time.Hour)
	id, _, _ := s.AddTodo("item", "", "", "", "x")
	got, _ := s.GetTodo(id)
	resp, err := c.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if want := `data-rev="1@` + got.UpdatedAt + `"`; !strings.Contains(readAll(t, resp), want) {
		t.Fatalf("row without %s", want)
	}
}

// Every action the page offers reaches the change feed, so another tab hears
// of it.
func TestPageActionsPublish(t *testing.T) {
	srv, s, c := eventsEnv(t, time.Hour)
	changes, stop := s.Subscribe()
	defer stop()
	post := func(path string, form url.Values, want store.Op) {
		t.Helper()
		resp, err := c.PostForm(srv.URL+path, form)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		select {
		case got := <-changes:
			if got.ID != 1 || got.Op != want {
				t.Fatalf("%s: got %+v, want #1 %s", path, got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s published nothing", path)
		}
	}

	post("/add", url.Values{"title": {"item"}, "scope": {"a"}}, store.Added)
	post("/todo/update", url.Values{"id": {"1"}, "title": {"item"}, "body": {"more"}, "scope": {"a"}}, store.Updated)
	post("/todo/close", url.Values{"id": {"1"}, "outcome": {"done"}}, store.Closed)
	post("/todo/reopen", url.Values{"id": {"1"}}, store.Reopened)
	post("/scope/rename", url.Values{"from": {"a"}, "to": {"b"}}, store.Updated)

	_, undo, err := s.CloseTodo(1, "dropped", "")
	if err != nil {
		t.Fatal(err)
	}
	<-changes
	post("/todo/undo", url.Values{"id": {"1"}, "undo": {undo}}, store.Reopened)
}
