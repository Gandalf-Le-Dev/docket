package store

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"strings"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAddAndDedupe(t *testing.T) {
	s := testStore(t)

	id, dup, err := s.AddTodo("  Fix the thing  ", "details", "  HopBox ", "repo:hopbox", "box-1")
	if err != nil || dup {
		t.Fatalf("add: id=%d dup=%v err=%v", id, dup, err)
	}
	got, err := s.GetTodo(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Fix the thing" || got.Scope != "hopbox" || got.Via != "box-1" || got.State != "open" {
		t.Fatalf("normalization wrong: %+v", got)
	}

	// Same normalized title+scope while open → the existing id, no new row.
	id2, dup, err := s.AddTodo("fix the THING", "", "hopbox", "elsewhere", "box-2")
	if err != nil || !dup || id2 != id {
		t.Fatalf("dedupe: id2=%d dup=%v err=%v", id2, dup, err)
	}

	// Different scope is a different item.
	id3, dup, err := s.AddTodo("Fix the thing", "", "other", "", "box-1")
	if err != nil || dup || id3 == id {
		t.Fatalf("scoped add: id3=%d dup=%v err=%v", id3, dup, err)
	}

	// Once closed, the same title files fresh — dedupe only guards open items.
	if _, err := s.CloseTodo(id, "done", ""); err != nil {
		t.Fatal(err)
	}
	id4, dup, err := s.AddTodo("Fix the thing", "", "hopbox", "", "box-1")
	if err != nil || dup || id4 == id {
		t.Fatalf("post-close add: id4=%d dup=%v err=%v", id4, dup, err)
	}
}

func TestValidation(t *testing.T) {
	s := testStore(t)
	var ve ValidationError

	if _, _, err := s.AddTodo("   ", "", "", "", "x"); !errors.As(err, &ve) {
		t.Fatalf("empty title: %v", err)
	}
	if _, _, err := s.AddTodo(strings.Repeat("t", MaxTitleBytes+1), "", "", "", "x"); !errors.As(err, &ve) {
		t.Fatalf("long title: %v", err)
	}
	if _, _, err := s.AddTodo("ok", strings.Repeat("b", MaxBodyBytes+1), "", "", "x"); !errors.As(err, &ve) {
		t.Fatalf("long body: %v", err)
	}
	if _, err := s.ListTodos("bogus", "", ""); !errors.As(err, &ve) {
		t.Fatalf("bad state: %v", err)
	}
	if _, err := s.GetTodo(999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing id: %v", err)
	}
}

func TestCloseReopenAndReason(t *testing.T) {
	s := testStore(t)
	id, _, _ := s.AddTodo("item", "original body", "", "", "x")

	closed, err := s.CloseTodo(id, "dropped", "not worth it")
	if err != nil {
		t.Fatal(err)
	}
	if closed.State != "dropped" || closed.ClosedAt == "" {
		t.Fatalf("close: %+v", closed)
	}
	if !strings.Contains(closed.Body, "original body") || !strings.Contains(closed.Body, "— closed (dropped): not worth it") {
		t.Fatalf("reason not appended: %q", closed.Body)
	}

	if _, err := s.CloseTodo(id, "bogus", ""); err == nil {
		t.Fatal("bad outcome accepted")
	}

	open := "open"
	reopened, err := s.UpdateTodo(id, TodoUpdate{State: &open})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.State != "open" || reopened.ClosedAt != "" {
		t.Fatalf("reopen: %+v", reopened)
	}
}

func TestListFilters(t *testing.T) {
	s := testStore(t)
	a, _, _ := s.AddTodo("alpha needle", "", "one", "", "x")
	s.AddTodo("beta", "with needle inside", "two", "", "x")
	s.AddTodo("gamma", "", "one", "", "x")
	s.CloseTodo(a, "done", "")

	open, _ := s.ListTodos("open", "", "")
	if len(open) != 2 {
		t.Fatalf("open: %d", len(open))
	}
	all, _ := s.ListTodos("all", "", "")
	if len(all) != 3 {
		t.Fatalf("all: %d", len(all))
	}
	scoped, _ := s.ListTodos("all", "ONE", "")
	if len(scoped) != 2 {
		t.Fatalf("scope filter should normalize: %d", len(scoped))
	}
	found, _ := s.ListTodos("all", "", "needle")
	if len(found) != 2 {
		t.Fatalf("query over title+body: %d", len(found))
	}
	// LIKE metacharacters in the query must not act as wildcards.
	none, _ := s.ListTodos("all", "", "%")
	if len(none) != 0 {
		t.Fatalf("unescaped LIKE: %d", len(none))
	}
}

func TestScopes(t *testing.T) {
	s := testStore(t)
	s.AddTodo("a", "", "hopbox", "", "x")
	s.AddTodo("b", "", "hopbox", "", "x")
	s.AddTodo("c", "", "hop-box", "", "x")

	scopes, err := s.Scopes()
	if err != nil || len(scopes) != 2 {
		t.Fatalf("scopes: %v %v", scopes, err)
	}
	if scopes[0].Scope != "hopbox" || scopes[0].Open != 2 {
		t.Fatalf("ordering by count: %+v", scopes)
	}

	n, err := s.RenameScope("hop-box", "hopbox")
	if err != nil || n != 1 {
		t.Fatalf("rename: n=%d err=%v", n, err)
	}
	scopes, _ = s.Scopes()
	if len(scopes) != 1 || scopes[0].Open != 3 {
		t.Fatalf("merged: %+v", scopes)
	}
}

func TestTokens(t *testing.T) {
	s := testStore(t)

	plain, err := s.CreateToken("box-1", "publish")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, "dkt_") || len(plain) != 4+32 {
		t.Fatalf("token shape: %q", plain)
	}

	name, role, err := s.Auth(plain)
	if err != nil || name != "box-1" || role != "publish" {
		t.Fatalf("auth: %s %s %v", name, role, err)
	}
	if _, _, err := s.Auth("dkt_wrong"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad token: %v", err)
	}

	// An active name cannot be silently replaced.
	if _, err := s.CreateToken("box-1", "publish"); err == nil {
		t.Fatal("duplicate active token accepted")
	}

	if err := s.RevokeToken("box-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Auth(plain); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked token still valid: %v", err)
	}
	if err := s.RevokeToken("box-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double revoke: %v", err)
	}

	// A revoked name can be re-minted (rotation), and the old plaintext stays dead.
	plain2, err := s.CreateToken("box-1", "review")
	if err != nil {
		t.Fatal(err)
	}
	if plain2 == plain {
		t.Fatal("rotation reused plaintext")
	}
	if _, role, err := s.Auth(plain2); err != nil || role != "review" {
		t.Fatalf("rotated auth: %s %v", role, err)
	}
	if _, _, err := s.Auth(plain); !errors.Is(err, ErrNotFound) {
		t.Fatal("old plaintext survived rotation")
	}
}

func TestScopeSummaries(t *testing.T) {
	s := testStore(t)
	for _, tc := range []struct{ title, scope, state string }{
		{"a", "docket", "open"}, {"b", "docket", "done"}, {"c", "docket", "dropped"},
		{"d", "thael", "open"}, {"e", "thael", "open"},
		{"f", "", "open"},
		{"g", "pilot", "done"},
	} {
		id, _, err := s.AddTodo(tc.title, "", tc.scope, "test", "t")
		if err != nil {
			t.Fatal(err)
		}
		if tc.state != "open" {
			if _, err := s.CloseTodo(id, tc.state, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := s.ScopeSummaries()
	if err != nil {
		t.Fatal(err)
	}
	want := []ScopeStats{
		{"", 1, 0, 0, 6},
		{"docket", 1, 1, 1, 1},
		{"pilot", 0, 1, 0, 7}, // all closed, still listed
		{"thael", 2, 0, 0, 4},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d: got %+v want %+v", i, got[i], want[i])
		}
	}
}

func tinyPNG(t *testing.T, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, c)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestImages(t *testing.T) {
	s := testStore(t)
	red := tinyPNG(t, color.RGBA{255, 0, 0, 255})

	id, err := s.AddImage(red)
	if err != nil {
		t.Fatal(err)
	}
	mime, data, err := s.GetImage(id)
	if err != nil || mime != "image/png" || !bytes.Equal(data, red) {
		t.Fatalf("get: %q %d bytes %v", mime, len(data), err)
	}

	// The same bytes again are the same image, not a second copy.
	if again, err := s.AddImage(red); err != nil || again != id {
		t.Fatalf("dedupe: %d %v", again, err)
	}
	blue, err := s.AddImage(tinyPNG(t, color.RGBA{0, 0, 255, 255}))
	if err != nil || blue == id {
		t.Fatalf("second image: %d %v", blue, err)
	}

	if _, _, err := s.GetImage(999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing image: %v", err)
	}
}

func TestImageValidation(t *testing.T) {
	s := testStore(t)
	var ve ValidationError
	for name, data := range map[string][]byte{
		"empty": nil,
		"text":  []byte("not a picture"),
		"svg":   []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"html":  []byte("<!doctype html><script>alert(1)</script>"),
		"huge":  append(tinyPNG(t, color.White), make([]byte, MaxImageBytes)...),
	} {
		if _, err := s.AddImage(data); !errors.As(err, &ve) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func TestImageRefs(t *testing.T) {
	body := "see ![a](/image/3) and ![b](/image/1)\n![again](/image/3) ![x](https://evil.example/a.png) ![y](/image/abc)"
	got := ImageRefs(body)
	if len(got) != 2 || got[0] != 3 || got[1] != 1 {
		t.Fatalf("ImageRefs = %v", got)
	}
}
