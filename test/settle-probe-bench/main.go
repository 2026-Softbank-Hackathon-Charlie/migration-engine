// Command settle-probe-bench compares real PostgreSQL settle observations.
// It never promotes, restarts, or changes a database. PG_BENCH_DSN must name a
// disposable fixture. The optional delay is applied to each forwarded TCP read,
// in each direction; it is not a claim about a production network's exact RTT.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/pg"
)

type observation struct {
	Recovery bool
	Pending  bool
	Started  time.Time
}

type sample struct {
	Delay       int     `json:"forwardDelayPerDirectionMillis"`
	Mode        string  `json:"mode"`
	Iteration   int     `json:"iteration"`
	QueryMillis float64 `json:"queryMillis"`
	TotalMillis float64 `json:"totalMillis"`
}

func main() {
	n := flag.Int("samples", 8, "alternating observations per mode and delay")
	flag.Parse()
	if *n < 1 {
		fail("samples must be positive")
	}
	u, err := url.Parse(os.Getenv("PG_BENCH_DSN"))
	if err != nil || u.Hostname() == "" || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		fail("PG_BENCH_DSN must be a disposable PostgreSQL URI")
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	upstream := net.JoinHostPort(u.Hostname(), port)
	enc := json.NewEncoder(os.Stdout)
	var all []sample
	for _, delay := range []int{0, 10, 25} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fail("start local latency proxy")
		}
		proxy := &latencyProxy{listener: listener, upstream: upstream, delay: time.Duration(delay) * time.Millisecond}
		acceptDone := make(chan struct{})
		go func() { defer close(acceptDone); proxy.serve() }()
		via := *u
		via.Host = listener.Addr().String()
		var expected *observation
		for i := 0; i < *n; i++ {
			modes := []string{"legacy", "combined"}
			if i%2 != 0 {
				modes[0], modes[1] = modes[1], modes[0]
			}
			for _, mode := range modes {
				started := time.Now()
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				db, err := pg.Connect(ctx, via.String())
				if err != nil {
					cancel()
					fail("connect to disposable PostgreSQL")
				}
				queryStart := time.Now()
				var got observation
				if mode == "legacy" {
					got.Recovery, err = db.InRecovery(ctx)
					if err == nil {
						got.Pending, err = db.PendingRestart(ctx)
					}
					if err == nil {
						got.Started, err = db.PostmasterStart(ctx)
					}
				} else {
					got.Recovery, got.Pending, got.Started, err = db.SettleStatus(ctx)
				}
				queryTook := time.Since(queryStart)
				db.Close()
				cancel()
				if err != nil {
					fail("read settle observation")
				}
				if expected != nil && (got.Recovery != expected.Recovery || got.Pending != expected.Pending || !got.Started.Equal(expected.Started)) {
					fail("fixture changed during A/B comparison")
				}
				expected = &got
				s := sample{Delay: delay, Mode: mode, Iteration: i, QueryMillis: float64(queryTook) / float64(time.Millisecond), TotalMillis: float64(time.Since(started)) / float64(time.Millisecond)}
				all = append(all, s)
				if enc.Encode(s) != nil {
					fail("write measurements")
				}
			}
		}
		listener.Close()
		<-acceptDone
		proxy.wg.Wait()
	}
	for _, delay := range []int{0, 10, 25} {
		for _, mode := range []string{"legacy", "combined"} {
			var query, total []float64
			for _, s := range all {
				if s.Delay == delay && s.Mode == mode {
					query = append(query, s.QueryMillis)
					total = append(total, s.TotalMillis)
				}
			}
			sort.Float64s(query)
			sort.Float64s(total)
			enc.Encode(map[string]any{"summary": true, "forwardDelayPerDirectionMillis": delay, "mode": mode, "samples": len(query), "queryMedianMillis": median(query), "totalMedianMillis": median(total), "queryP95Millis": percentile(query, 0.95), "totalP95Millis": percentile(total, 0.95)})
		}
	}
}

func fail(stage string) { fmt.Fprintln(os.Stderr, stage); os.Exit(1) }
func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	if len(v)%2 == 0 {
		return (v[len(v)/2-1] + v[len(v)/2]) / 2
	}
	return v[len(v)/2]
}
func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	i := int(float64(len(v)) * p)
	if i >= len(v) {
		i = len(v) - 1
	}
	return v[i]
}

type latencyProxy struct {
	listener net.Listener
	upstream string
	delay    time.Duration
	wg       sync.WaitGroup
}

func (p *latencyProxy) serve() {
	for {
		downstream, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			upstream, err := net.DialTimeout("tcp", p.upstream, 5*time.Second)
			if err != nil {
				downstream.Close()
				return
			}
			defer downstream.Close()
			defer upstream.Close()
			done := make(chan struct{})
			go func() {
				p.forward(upstream, downstream)
				if c, ok := upstream.(*net.TCPConn); ok {
					c.CloseWrite()
				}
				close(done)
			}()
			p.forward(downstream, upstream)
			<-done
		}()
	}
}
func (p *latencyProxy) forward(dst, src net.Conn) {
	b := make([]byte, 32*1024)
	for {
		n, err := src.Read(b)
		if n > 0 {
			if p.delay > 0 {
				time.Sleep(p.delay)
			}
			if _, werr := io.Copy(dst, bytes.NewReader(b[:n])); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
