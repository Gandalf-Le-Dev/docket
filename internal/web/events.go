package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"
)

// heartbeat keeps an idle stream from looking dead to a proxy, most of which
// give up on a quiet connection after a minute or so.
const heartbeat = 25 * time.Second

// events streams the store's changes to a signed-in page as Server-Sent
// Events: which item, and whether it was added, updated, closed or reopened,
// never its text, with the store.Seq it left. The page compares that with the
// Seq it was rendered at and refreshes itself from the ordinary pages.
//
// Signed out, it answers 204, the one answer that stops EventSource from
// reconnecting; the page then loads itself and lands on the login page. The
// session is checked again at each heartbeat, so a revoked token's stream
// ends within one.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	token := cookie(r, cookieName)
	if !h.signedIn(token) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	rc := http.NewResponseController(w)
	// A server WriteTimeout would otherwise cut every stream off at its length.
	if err := rc.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		log.Printf("web: events: clear write deadline: %v", err)
	}
	changes, stop := h.store.Subscribe()
	defer stop()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	// nginx holds a response back until it is done unless told not to.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// Sent at once, it tells the page whether it missed anything before the
	// stream opened, and gets bytes past a proxy that holds back headers.
	if _, err := fmt.Fprintf(w, "event: seq\ndata: %s\n\n", h.store.Seq()); err != nil {
		return
	}
	if err := rc.Flush(); err != nil {
		return
	}

	beat := time.NewTicker(h.beat)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case c := <-changes:
			data, err := json.Marshal(c)
			if err != nil {
				log.Printf("web: events: encode %+v: %v", c, err)
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
		case <-beat.C:
			if !h.signedIn(token) {
				return
			}
			if _, err := fmt.Fprint(w, ": beat\n\n"); err != nil {
				return
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}
