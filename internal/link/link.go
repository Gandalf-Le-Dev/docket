// Package link decides what URL an item has. Docket serves the item page
// itself, at /todo/{id}; the origin is either configured (-public-url) or
// taken from the request the way a reverse proxy presents it.
package link

import (
	"fmt"
	"net/http"
	"strings"
)

// HTTPS reports whether the request reached Docket over TLS, directly or via
// a proxy that says so. First value only: proxy chains append to the header.
func HTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	return strings.TrimSpace(proto) == "https"
}

// Base is the origin items are addressed from: the configured public URL
// when there is one, otherwise the scheme and host this request arrived on,
// honoring X-Forwarded-Proto and X-Forwarded-Host from a reverse proxy.
func Base(r *http.Request, configured string) string {
	if configured != "" {
		return strings.TrimRight(configured, "/")
	}
	scheme := "http"
	if HTTPS(r) {
		scheme = "https"
	}
	host, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Host"), ",")
	host = strings.TrimSpace(host)
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

// Item is the canonical URL of one item under base.
func Item(base string, id int64) string {
	return fmt.Sprintf("%s/todo/%d", base, id)
}
