package store

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// setClock pins the store's clock at start and returns a way to move it.
func setClock(t *testing.T, start time.Time) func(time.Duration) {
	t.Helper()
	at := start
	clock = func() time.Time { return at }
	t.Cleanup(func() { clock = time.Now })
	return func(d time.Duration) { at = at.Add(d) }
}

func seenAt(t *testing.T, s *Store, id int64) string {
	t.Helper()
	var seen string
	if err := s.db.QueryRow("SELECT seen_at FROM image WHERE id = ?", id).Scan(&seen); err != nil {
		t.Fatal(err)
	}
	return seen
}

// Pasting an image again counts as seeing it now, so an old copy cannot be
// treated as abandoned under a fresh draft.
func TestAddImageMarksSeen(t *testing.T) {
	s := testStore(t)
	advance := setClock(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	pic := tinyPNG(t, color.Black)
	id, err := s.AddImage(pic)
	if err != nil {
		t.Fatal(err)
	}
	if got := seenAt(t, s, id); got != "2026-01-01T00:00:00Z" {
		t.Fatalf("seen_at on insert = %s", got)
	}
	advance(30 * 24 * time.Hour)
	if again, err := s.AddImage(pic); err != nil || again != id {
		t.Fatalf("dedupe: %d %v", again, err)
	}
	if got := seenAt(t, s, id); got != "2026-01-31T00:00:00Z" {
		t.Fatalf("seen_at after dedupe = %s", got)
	}
}

func TestPruneImages(t *testing.T) {
	const grace = 7 * 24 * time.Hour
	const day = 24 * time.Hour
	s := testStore(t)
	advance := setClock(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	add := func(shade uint8) int64 {
		t.Helper()
		id, err := s.AddImage(tinyPNG(t, color.Gray{Y: shade}))
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	marker := func(id int64) string { return fmt.Sprintf("![image](/image/%d)", id) }
	prune := func(want int) {
		t.Helper()
		if n, err := s.PruneImages(grace); err != nil || n != want {
			t.Fatalf("prune: deleted %d, want %d (err %v)", n, want, err)
		}
	}
	exists := func(id int64) bool {
		t.Helper()
		_, _, err := s.GetImage(id)
		if err != nil && !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		return err == nil
	}

	inOpen, inClosed, abandoned, repasted, removed := add(1), add(2), add(3), add(4), add(5)
	s.AddTodo("open", marker(inOpen), "", "", "x")
	closed, _, _ := s.AddTodo("closed", marker(inClosed), "", "", "x")
	s.CloseTodo(closed, "done", "")
	edited, _, _ := s.AddTodo("edited", "before "+marker(removed), "", "", "x")

	// Just short of the grace nothing goes. This sweep still sees removed's
	// marker, so removed's grace restarts here.
	advance(grace - time.Second)
	prune(0)

	// Two days later abandoned and repasted are past the grace, but repasted
	// is pasted again first, which restarts its grace.
	advance(2 * day)
	if _, err := s.AddImage(tinyPNG(t, color.Gray{Y: 4})); err != nil {
		t.Fatal(err)
	}
	body := "after, image gone"
	if _, err := s.UpdateTodo(edited, TodoUpdate{Body: &body}); err != nil {
		t.Fatal(err)
	}
	prune(1)
	if exists(abandoned) {
		t.Fatal("abandoned image survived its grace")
	}
	for name, id := range map[string]int64{"open": inOpen, "closed": inClosed, "repasted": repasted, "removed": removed} {
		if !exists(id) {
			t.Fatalf("%s image deleted", name)
		}
	}

	// removed goes one grace after the last sweep that saw its marker.
	advance(grace - 2*day - time.Second)
	prune(0)
	advance(2 * time.Second)
	prune(1)
	if exists(removed) {
		t.Fatal("removed image outlived its grace")
	}

	// repasted goes one grace after it was pasted again.
	advance(2 * day)
	prune(1)
	if exists(repasted) {
		t.Fatal("repasted image outlived its grace")
	}
	if !exists(inOpen) || !exists(inClosed) {
		t.Fatal("referenced images deleted")
	}

	if _, err := s.PruneImages(0); err == nil {
		t.Fatal("zero grace accepted")
	}
}

// A browser caches /image/N forever, so a pruned id must never come back
// holding another picture.
func TestPrunedImageIDNotReused(t *testing.T) {
	s := testStore(t)
	advance := setClock(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if _, err := s.AddImage(tinyPNG(t, color.Gray{Y: 1})); err != nil {
		t.Fatal(err)
	}
	last, err := s.AddImage(tinyPNG(t, color.Gray{Y: 2}))
	if err != nil {
		t.Fatal(err)
	}
	advance(2 * time.Hour)
	if n, err := s.PruneImages(time.Hour); err != nil || n != 2 {
		t.Fatalf("prune: %d %v", n, err)
	}
	next, err := s.AddImage(tinyPNG(t, color.Gray{Y: 3}))
	if err != nil {
		t.Fatal(err)
	}
	if next <= last {
		t.Fatalf("new image got id %d, pruned ids went up to %d", next, last)
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
