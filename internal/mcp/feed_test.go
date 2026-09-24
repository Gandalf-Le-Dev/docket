package mcp

import (
	"testing"
	"time"

	"github.com/Gandalf-Le-Dev/docket/internal/store"
)

// An agent's writes reach open pages the same way the page's own do.
func TestToolWritesPublish(t *testing.T) {
	e := newEnv(t)
	changes, stop := e.store.Subscribe()
	defer stop()
	want := func(op store.Op) {
		t.Helper()
		select {
		case c := <-changes:
			if c.ID != 1 || c.Op != op {
				t.Fatalf("got %+v, want #1 %s", c, op)
			}
		case <-time.After(time.Second):
			t.Fatalf("no %s change published", op)
		}
	}
	call := func(token, name, args string) {
		t.Helper()
		headers, body := modernCall(name, args)
		_, r := e.call(t, token, headers, body)
		structured(t, r)
	}

	call(e.publish, "todo_add", `{"title":"from an agent"}`)
	want(store.Added)
	call(e.review, "todo_update", `{"id":1,"body":"more detail"}`)
	want(store.Updated)
	call(e.review, "todo_close", `{"id":1,"outcome":"done"}`)
	want(store.Closed)
	call(e.review, "todo_update", `{"id":1,"state":"open"}`)
	want(store.Reopened)

	call(e.review, "todo_list", `{}`)
	call(e.review, "todo_get", `{"id":1}`)
	select {
	case c := <-changes:
		t.Fatalf("a read published %+v", c)
	default:
	}
}
