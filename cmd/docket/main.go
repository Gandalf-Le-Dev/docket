// Docket: a self-hosted backlog any agent can write to.
//
//	docket serve                      run the server (MCP + web + healthz)
//	docket token new -role R NAME     mint a token; prints the plaintext once
//	docket token revoke NAME          revoke a token
//	docket token list                 list tokens
//	docket version                    print the version
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/Gandalf-Le-Dev/docket/internal/mcp"
	"github.com/Gandalf-Le-Dev/docket/internal/store"
	"github.com/Gandalf-Le-Dev/docket/internal/web"
)

// version is stamped by the build (-ldflags "-X main.version=...").
var version = "dev"

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func dbFlag(fs *flag.FlagSet) *string {
	return fs.String("db", envOr("DOCKET_DB", "/var/lib/docket/docket.db"),
		"path to the SQLite database (env: DOCKET_DB)")
}

func usage() {
	fmt.Fprintf(os.Stderr, `docket %s — a self-hosted backlog any agent can write to

Usage:
  docket serve [-db PATH] [-addr ADDR] [-public-url URL]
  docket token new -role publish|review NAME
  docket token revoke NAME
  docket token list
  docket version
`, version)
	os.Exit(2)
}

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "token":
		token(os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		usage()
	}
}

func openStore(path string) *store.Store {
	s, err := store.Open(path)
	if err != nil {
		log.Fatalf("docket: open %s: %v", path, err)
	}
	return s
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	db := dbFlag(fs)
	addr := fs.String("addr", envOr("DOCKET_ADDR", ":8340"), "listen address (env: DOCKET_ADDR)")
	publicURL := fs.String("public-url", envOr("DOCKET_PUBLIC_URL", ""),
		"origin item links are built on, e.g. https://docket.example.net; "+
			"defaults to each request's own scheme and host (env: DOCKET_PUBLIC_URL)")
	fs.Parse(args)

	s := openStore(*db)
	defer s.Close()

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewHandler(s, version, *publicURL))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// Answers 200 only if the database responds — a wedged SQLite file
		// fails the deploy health check rather than going live dead.
		if err := s.Ping(); err != nil {
			http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.Handle("/", web.NewHandler(s))

	log.Printf("docket %s listening on %s (db %s)", version, *addr, *db)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatalf("docket: %v", err)
	}
}

func token(args []string) {
	if len(args) < 1 {
		usage()
	}
	switch args[0] {
	case "new":
		fs := flag.NewFlagSet("token new", flag.ExitOnError)
		db := dbFlag(fs)
		role := fs.String("role", "", "publish (add-only, one per agent box) or review (full access)")
		fs.Parse(args[1:])
		if fs.NArg() != 1 || *role == "" {
			log.Fatal("usage: docket token new -role publish|review NAME")
		}
		name := fs.Arg(0)
		s := openStore(*db)
		defer s.Close()
		plaintext, err := s.CreateToken(name, *role)
		if err != nil {
			log.Fatalf("docket: %v", err)
		}
		// Printed here, on demand, and never to the service log: "secrets
		// don't go to logs" should not depend on a redactor winning.
		fmt.Printf("%s\n", plaintext)
		fmt.Fprintf(os.Stderr, "token %q (%s) created — this plaintext is shown exactly once\n", name, *role)

	case "revoke":
		fs := flag.NewFlagSet("token revoke", flag.ExitOnError)
		db := dbFlag(fs)
		fs.Parse(args[1:])
		if fs.NArg() != 1 {
			log.Fatal("usage: docket token revoke NAME")
		}
		s := openStore(*db)
		defer s.Close()
		if err := s.RevokeToken(fs.Arg(0)); err != nil {
			log.Fatalf("docket: %v", err)
		}
		fmt.Fprintf(os.Stderr, "token %q revoked\n", fs.Arg(0))

	case "list":
		fs := flag.NewFlagSet("token list", flag.ExitOnError)
		db := dbFlag(fs)
		fs.Parse(args[1:])
		s := openStore(*db)
		defer s.Close()
		tokens, err := s.ListTokens()
		if err != nil {
			log.Fatalf("docket: %v", err)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tROLE\tCREATED\tSTATUS")
		for _, t := range tokens {
			status := "active"
			if t.Revoked {
				status = "revoked"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", t.Name, t.Role, t.CreatedAt, status)
		}
		tw.Flush()

	default:
		usage()
	}
}
