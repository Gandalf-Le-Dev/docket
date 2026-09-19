package link

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"
)

func TestBase(t *testing.T) {
	cases := []struct {
		name       string
		configured string
		headers    map[string]string
		tls        bool
		want       string
	}{
		{name: "plain host", want: "http://docket.local:8340"},
		{name: "direct tls", tls: true, want: "https://docket.local:8340"},
		{name: "behind proxy",
			headers: map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "docket.example.net"},
			want:    "https://docket.example.net"},
		{name: "proxy chain takes the first hop",
			headers: map[string]string{"X-Forwarded-Proto": "https, http", "X-Forwarded-Host": "docket.example.net, internal"},
			want:    "https://docket.example.net"},
		{name: "configured wins and loses its slash",
			configured: "https://todo.example.org/",
			headers:    map[string]string{"X-Forwarded-Host": "spoofed"},
			want:       "https://todo.example.org"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://docket.local:8340/mcp", nil)
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			if c.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if got := Base(r, c.configured); got != c.want {
				t.Fatalf("Base = %q, want %q", got, c.want)
			}
		})
	}
}

func TestItem(t *testing.T) {
	if got := Item("https://todo.example.org", 42); got != "https://todo.example.org/todo/42" {
		t.Fatal(got)
	}
}
