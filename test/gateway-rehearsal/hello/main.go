// Command hello is the smallest app for the gateway rehearsal. Each instance
// talks to one database (DATABASE_URL); the gateway in front decides which
// instance gets the writes.
//
//	POST /visit  insert one row into visits, answer {"id","db","inRecovery"}
//	GET  /       visits count and which database answered
//	GET  /healthz 200 when the database answers SELECT 1
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "listen address")
	name := flag.String("name", "hello", "instance name shown in answers")
	flag.Parse()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		log.Fatal("bad DATABASE_URL")
	}
	cfg.MaxConns = 4
	cfg.HealthCheckPeriod = 2 * time.Second
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		log.Fatal(err)
	}
	ensured := false
	ensure := func(ctx context.Context) error {
		if ensured {
			return nil
		}
		_, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS visits (id bigserial PRIMARY KEY, at timestamptz NOT NULL DEFAULT now(), via text NOT NULL)`)
		if err == nil {
			ensured = true
		}
		return err
	}

	reply := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, "SELECT 1"); err != nil {
			reply(w, 503, map[string]string{"instance": *name, "error": "database unavailable"})
			return
		}
		reply(w, 200, map[string]string{"instance": *name})
	})
	mux.HandleFunc("POST /visit", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := ensure(ctx); err != nil {
			reply(w, 503, map[string]string{"instance": *name, "error": err.Error()})
			return
		}
		var id int64
		var db string
		var rec bool
		err := pool.QueryRow(ctx, `INSERT INTO visits (via) VALUES ($1) RETURNING id, current_setting('cluster_name'), pg_is_in_recovery()`, *name).Scan(&id, &db, &rec)
		if err != nil {
			reply(w, 503, map[string]string{"instance": *name, "error": err.Error()})
			return
		}
		reply(w, 200, map[string]any{"id": id, "instance": *name, "db": db, "inRecovery": rec})
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var n int64
		var db string
		var rec bool
		err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM visits), current_setting('cluster_name'), pg_is_in_recovery()`).Scan(&n, &db, &rec)
		if err != nil {
			reply(w, 503, map[string]string{"instance": *name, "error": err.Error()})
			return
		}
		reply(w, 200, map[string]any{"hello": "world", "visits": n, "instance": *name, "db": db, "inRecovery": rec})
	})
	log.Printf("hello %s listening on %s", *name, *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}
