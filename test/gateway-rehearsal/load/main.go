// Command load sends POST /visit through the gateway at a fixed rate until
// the stop file appears. Every 2xx answer is appended to -written as
// {"table":"visits","pk":"<id>"} (the engine's verify.writtenRecords format);
// every request is appended to -log with its status and latency.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

type answer struct {
	ID         int64  `json:"id"`
	DB         string `json:"db"`
	InRecovery bool   `json:"inRecovery"`
}

type entry struct {
	At        string `json:"at"`
	Status    int    `json:"status"`
	LatencyMs int64  `json:"latencyMs"`
	ID        int64  `json:"id,omitempty"`
	DB        string `json:"db,omitempty"`
	Err       string `json:"err,omitempty"`
}

func main() {
	url := flag.String("url", "http://127.0.0.1:55580", "gateway proxy URL")
	every := flag.Duration("every", 100*time.Millisecond, "time between requests per worker")
	workers := flag.Int("workers", 2, "concurrent workers")
	written := flag.String("written", "written.jsonl", "2xx ids, engine format")
	logPath := flag.String("log", "requests.jsonl", "every request")
	stop := flag.String("stop", "load.stop", "stop when this file exists")
	summary := flag.String("summary", "load-summary.json", "summary written on stop")
	flag.Parse()

	wf, err := os.OpenFile(*written, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	lf, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	client := &http.Client{Timeout: 120 * time.Second}

	var mu sync.Mutex
	statuses := map[int]int{}
	byDB := map[string]int{}
	var lat []int64
	total := 0

	var wg sync.WaitGroup
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if _, err := os.Stat(*stop); err == nil {
					return
				}
				start := time.Now()
				e := entry{At: start.UTC().Format(time.RFC3339Nano)}
				resp, err := client.Post(*url+"/visit", "application/json", nil)
				e.LatencyMs = time.Since(start).Milliseconds()
				if err != nil {
					e.Err = "transport error"
				} else {
					e.Status = resp.StatusCode
					body, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					var a answer
					if resp.StatusCode/100 == 2 && json.Unmarshal(body, &a) == nil && a.ID > 0 {
						e.ID, e.DB = a.ID, a.DB
					} else if resp.StatusCode/100 != 2 {
						if len(body) > 200 {
							body = body[:200]
						}
						e.Err = string(body)
					}
				}
				line, _ := json.Marshal(e)
				mu.Lock()
				total++
				statuses[e.Status]++
				lat = append(lat, e.LatencyMs)
				fmt.Fprintln(lf, string(line))
				if e.ID > 0 {
					byDB[e.DB]++
					fmt.Fprintf(wf, "{\"table\":\"visits\",\"pk\":\"%d\"}\n", e.ID)
				}
				mu.Unlock()
				if d := *every - time.Since(start); d > 0 {
					time.Sleep(d)
				}
			}
		}()
	}
	wg.Wait()
	wf.Close()
	lf.Close()

	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) int64 {
		if len(lat) == 0 {
			return 0
		}
		return lat[int(float64(len(lat)-1)*p)]
	}
	ok := 0
	for s, n := range statuses {
		if s/100 == 2 {
			ok += n
		}
	}
	out := map[string]any{
		"total": total, "ok": ok, "failed": total - ok, "statuses": statuses, "writesByDB": byDB,
		"latencyMs": map[string]int64{"p50": pct(0.5), "p99": pct(0.99), "max": pct(1)},
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	_ = os.WriteFile(*summary, b, 0o644)
	fmt.Println(string(b))
}
