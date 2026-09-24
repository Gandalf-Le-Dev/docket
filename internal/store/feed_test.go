package store

import (
	"fmt"
	"image/color"
	"testing"
	"time"
)

func next(t *testing.T, changes <-chan Change) Change {
	t.Helper()
	select {
	case c := <-changes:
		return c
	case <-time.After(time.Second):
		t.Fatal("no change published")
		return Change{}
	}
}

func quiet(t *testing.T, changes <-chan Change) {
	t.Helper()
	select {
	case c := <-changes:
		t.Fatalf("unexpected change %+v", c)
	default:
	}
}

func TestEveryWritePublishes(t *testing.T) {
	s := testStore(t)
	changes, stop := s.Subscribe()
	defer stop()

	id, _, err := s.AddTodo("item", "", "a", "", "x")
	if err != nil {
		t.Fatal(err)
	}
	want := func(op Op) {
		t.Helper()
		if c := next(t, changes); c != (Change{ID: id, Op: op}) {
			t.Fatalf("got %+v, want #%d %s", c, id, op)
		}
		quiet(t, changes)
	}
	want(Added)

	if _, dup, _ := s.AddTodo("item", "", "a", "", "x"); !dup {
		t.Fatal("not a duplicate")
	}
	quiet(t, changes)

	title := "renamed"
	if _, err := s.UpdateTodo(id, TodoUpdate{Title: &title}); err != nil {
		t.Fatal(err)
	}
	want(Updated)

	_, undo, err := s.CloseTodo(id, "done", "")
	if err != nil {
		t.Fatal(err)
	}
	want(Closed)

	if _, err := s.UndoClose(id, undo); err != nil {
		t.Fatal(err)
	}
	want(Reopened)

	dropped := "dropped"
	if _, err := s.UpdateTodo(id, TodoUpdate{State: &dropped}); err != nil {
		t.Fatal(err)
	}
	want(Closed)

	open := "open"
	if _, err := s.UpdateTodo(id, TodoUpdate{State: &open}); err != nil {
		t.Fatal(err)
	}
	want(Reopened)

	if _, err := s.RenameScope("a", "b"); err != nil {
		t.Fatal(err)
	}
	want(Updated)
}

func TestFailedWritesPublishNothing(t *testing.T) {
	s := testStore(t)
	id, _, _ := s.AddTodo("item", "", "", "", "x")
	changes, stop := s.Subscribe()
	defer stop()

	if _, _, err := s.AddTodo("", "", "", "", "x"); err == nil {
		t.Fatal("empty title accepted")
	}
	empty := ""
	if _, err := s.UpdateTodo(id, TodoUpdate{Title: &empty}); err == nil {
		t.Fatal("empty title accepted")
	}
	if _, _, err := s.CloseTodo(999, "done", ""); err == nil {
		t.Fatal("closed a missing item")
	}
	if _, err := s.UndoClose(id, "1"); err == nil {
		t.Fatal("undid a close that never happened")
	}
	if n, err := s.RenameScope("nothing", "else"); err != nil || n != 0 {
		t.Fatalf("rename of an unused scope: %d, %v", n, err)
	}
	if _, err := s.AddImage(tinyPNG(t, color.White)); err != nil {
		t.Fatal(err)
	}
	quiet(t, changes)
}

// A subscriber that never reads must not hold up the writers.
func TestFullSubscriberNeverBlocks(t *testing.T) {
	s := testStore(t)
	_, stop := s.Subscribe()
	defer stop()
	done := make(chan error)
	go func() {
		for i := range subBuffer * 3 {
			if _, _, err := s.AddTodo(fmt.Sprintf("item %d", i), "", "", "", "x"); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a full subscriber blocked a write")
	}
}

func TestStopUnsubscribes(t *testing.T) {
	s := testStore(t)
	_, stop1 := s.Subscribe()
	changes, stop2 := s.Subscribe()
	if n := s.Subscribers(); n != 2 {
		t.Fatalf("%d subscribers, want 2", n)
	}
	stop1()
	stop1()
	if n := s.Subscribers(); n != 1 {
		t.Fatalf("%d subscribers after stop, want 1", n)
	}
	s.AddTodo("item", "", "", "", "x")
	next(t, changes)
	stop2()
	if n := s.Subscribers(); n != 0 {
		t.Fatalf("%d subscribers after both stopped", n)
	}
}
