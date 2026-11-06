// coldapp stands in for an app that touches its schema every time it starts
// and has no switch to turn that off (ADR 0012). It runs goose Up, then
// applies its bootstrap DDL with CREATE ... IF NOT EXISTS on every start (the
// pattern moimroom used before it learned to skip it on a standby). goose
// alone writes nothing when no migration is pending; the IF NOT EXISTS DDL is
// rejected on a read-only replica even when the objects exist, so the app
// exits there.
//
//	GET  /healthz       200 (readiness)
//	GET  /              which environment answered
//	POST /visits?id=N   insert one row
package main

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrations embed.FS

func main() {
	env, addr := os.Getenv("APP_ENV"), os.Getenv("LISTEN")
	if addr == "" {
		addr = ":8080"
	}
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		log.Fatalf("migrate on start: %v", err)
	}
	// Bootstrap DDL on every start; PostgreSQL refuses it in a read-only
	// transaction before it looks at whether the objects exist.
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS app_settings (key text PRIMARY KEY, value text NOT NULL)`); err != nil {
		log.Fatalf("bootstrap schema on start: %v", err)
	}
	cancel()
	v, _ := goose.GetDBVersion(db)
	log.Printf("%s: schema at version %d", env, v)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, "coldapp %s\n", env) })
	mux.HandleFunc("POST /visits", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		if err != nil {
			http.Error(w, "id", http.StatusBadRequest)
			return
		}
		c, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, err := db.ExecContext(c, "insert into visits (id, at, via) values ($1, now(), $2)", id, env); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	log.Printf("%s listening on %s", env, addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
