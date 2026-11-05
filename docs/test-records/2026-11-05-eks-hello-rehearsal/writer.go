// writer inserts one row every -every into whichever of two databases accepts
// writes (like an app that reconnects to the new primary), records every
// acknowledged id, and on SIGINT/-for expiry reports the longest gap between
// acknowledged writes. "check" mode verifies every acknowledged id is on a
// database.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

func main() {
	a := flag.String("a", os.Getenv("DSN_A"), "first DSN")
	b := flag.String("b", os.Getenv("DSN_B"), "second DSN")
	every := flag.Duration("every", 100*time.Millisecond, "write interval")
	dur := flag.Duration("for", 0, "stop after this long (0 = until interrupted)")
	acked := flag.String("acked", "acked.txt", "file of acknowledged ids")
	check := flag.String("check", "", "DSN to verify acknowledged ids against, then exit")
	url := flag.String("url", "", "write through the gateway: POST <url>/visits?id=N (instead of -a/-b)")
	flag.Parse()

	if *check != "" {
		os.Exit(verify(*check, *acked))
	}
	if *url != "" {
		os.Exit(viaHTTP(*url, *every, *dur, *acked))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *dur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *dur)
		defer cancel()
	}
	f, err := os.OpenFile(*acked, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()

	dsns := []string{*a, *b}
	names := []string{"a", "b"}
	conns := make([]*pgx.Conn, 2)
	cur := 0
	// Ids never collide with an earlier round: start from the current time in ms x 1000.
	id := time.Now().UnixMilli() * 1000
	var ok, failed int
	var lastOK, gapStart time.Time
	var longest time.Duration
	start := time.Now()
	lastOK = start
	lastLog := start

	write := func(i int) error {
		if conns[i] == nil {
			c, err := pgx.Connect(ctx, dsns[i])
			if err != nil {
				return err
			}
			conns[i] = c
		}
		wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_, err := conns[i].Exec(wctx, "insert into visits (id, at, via) values ($1, now(), $2)", id, names[i])
		if err != nil {
			conns[i].Close(context.Background())
			conns[i] = nil
		}
		return err
	}

	t := time.NewTicker(*every)
	defer t.Stop()
	for ctx.Err() == nil {
		id++
		err := write(cur)
		if err != nil && ctx.Err() == nil {
			other := 1 - cur
			if err2 := write(other); err2 == nil {
				fmt.Printf("%s writes moved %s -> %s (%v)\n", time.Now().Format("15:04:05.000"), names[cur], names[other], short(err))
				cur, err = other, nil
			}
		}
		now := time.Now()
		if err == nil {
			ok++
			fmt.Fprintln(f, id)
			if g := now.Sub(lastOK); g > longest {
				longest, gapStart = g, lastOK
			}
			lastOK = now
		} else if ctx.Err() == nil {
			failed++
		}
		if now.Sub(lastLog) >= 10*time.Second {
			fmt.Printf("%s ok=%d failed=%d writing to %s\n", now.Format("15:04:05"), ok, failed, names[cur])
			lastLog = now
		}
		select {
		case <-ctx.Done():
		case <-t.C:
		}
	}
	fmt.Printf("RESULT ok=%d failed=%d longest_gap=%s (from %s) ran=%s last=%s\n",
		ok, failed, longest.Round(time.Millisecond), gapStart.Format("15:04:05.000"), time.Since(start).Round(time.Second), names[cur])
}

// viaHTTP writes one row per tick through the gateway, one request at a time
// like a single client. A held write simply takes longer; the longest gap
// between acknowledged writes is what a user saw.
func viaHTTP(base string, every, dur time.Duration, path string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if dur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, dur)
		defer cancel()
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer f.Close()
	client := &http.Client{Timeout: 70 * time.Second}
	id := time.Now().UnixMilli() * 1000
	var ok, failed int
	var longest, slowest time.Duration
	var gapStart time.Time
	start := time.Now()
	lastOK, lastLog := start, start
	codes := map[string]int{}
	t := time.NewTicker(every)
	defer t.Stop()
	for ctx.Err() == nil {
		id++
		sent := time.Now()
		resp, err := client.Post(fmt.Sprintf("%s/visits?id=%d", base, id), "text/plain", nil)
		took := time.Since(sent)
		now := time.Now()
		if err == nil {
			backend := resp.Header.Get("X-Charlie-Backend")
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			codes[fmt.Sprintf("%d %s", resp.StatusCode, backend)]++
			if resp.StatusCode == http.StatusCreated {
				ok++
				fmt.Fprintln(f, id)
				if g := now.Sub(lastOK); g > longest {
					longest, gapStart = g, lastOK
				}
				if took > slowest {
					slowest = took
				}
				lastOK = now
			} else {
				failed++
			}
		} else if ctx.Err() == nil {
			failed++
			codes["error"]++
		}
		if now.Sub(lastLog) >= 10*time.Second {
			fmt.Printf("%s ok=%d failed=%d\n", now.Format("15:04:05"), ok, failed)
			lastLog = now
		}
		select {
		case <-ctx.Done():
		case <-t.C:
		}
	}
	fmt.Printf("RESULT ok=%d failed=%d longest_gap=%s (from %s) slowest_write=%s ran=%s responses=%v\n",
		ok, failed, longest.Round(time.Millisecond), gapStart.Format("15:04:05.000"), slowest.Round(time.Millisecond), time.Since(start).Round(time.Second), codes)
	return 0
}

func short(err error) string {
	s := err.Error()
	if len(s) > 90 {
		s = s[:90]
	}
	return s
}

func verify(dsn, path string) int {
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer c.Close(ctx)
	have := map[int64]bool{}
	rows, err := c.Query(ctx, "select id from visits")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		have[id] = true
	}
	if rows.Err() != nil {
		fmt.Fprintln(os.Stderr, rows.Err())
		return 1
	}
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer f.Close()
	var n, missing int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		id, _ := strconv.ParseInt(sc.Text(), 10, 64)
		n++
		if !have[id] {
			missing++
			if missing <= 10 {
				fmt.Println("missing id", id)
			}
		}
	}
	fmt.Printf("CHECK acknowledged=%d present=%d missing=%d (rows in table %d)\n", n, n-missing, missing, len(have))
	if missing > 0 {
		return 1
	}
	return 0
}
