// mload sends moimroom user traffic to a public URL (through the traffic
// gateway) for a fixed time: bookers reserve free one-hour slots, a host
// uploads a small unique PNG every interval, readers fetch spaces and photos.
// Every booking it makes has guestName "migration-rehearsal" so it can be
// cleaned up. It writes the ids of successful bookings and photos to -out and,
// after the run fetches every uploaded photo again and compares its SHA-256.
//
// Adapted from platform/test/e2e/moimroom/driver (load only, no platform API).
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const guest = "migration-rehearsal"

var kst = time.FixedZone("KST", 9*3600)

type space struct {
	ID        int64 `json:"id"`
	OpenHour  int   `json:"openHour"`
	CloseHour int   `json:"closeHour"`
}

type slot struct {
	space int64
	date  string
	hour  int
}

type photo struct {
	Space int64  `json:"space"`
	ID    int64  `json:"id"`
	Size  int    `json:"size"`
	SHA   string `json:"sha256"`
}

type load struct {
	gw, hostCode string
	hc           *http.Client
	spaces       []space
	slots        []slot
	next         atomic.Int64

	mu        sync.Mutex
	ids       []int64
	photos    []photo
	lastOK    time.Time
	maxGap    time.Duration
	gapAt     time.Time
	slowest   time.Duration
	byBackend map[string]int64
	failLogs  int

	writeOK, writeFail, conflict, photoOK, photoFail, readOK, readMissing, readFail atomic.Int64
}

func main() {
	gw := flag.String("gateway", "https://moimroom.hopcharlie.dev", "public URL")
	dur := flag.Duration("for", 4*time.Minute, "how long to send traffic")
	writers := flag.Int("writers", 2, "concurrent booking users")
	readers := flag.Int("readers", 2, "concurrent reading users")
	photoEvery := flag.Duration("photo-every", 3*time.Second, "how often the host uploads a photo")
	out := flag.String("out", "mload", "file prefix for results")
	flag.Parse()
	hostCode := os.Getenv("HOST_CODE")
	if hostCode == "" {
		log.Fatal("HOST_CODE is required")
	}
	l := &load{gw: strings.TrimRight(*gw, "/"), hostCode: hostCode,
		hc:        &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{MaxIdleConnsPerHost: 64}},
		byBackend: map[string]int64{}}
	if err := l.loadSpaces(); err != nil {
		log.Fatal(err)
	}
	log.Printf("%d spaces, %d free slots; traffic for %s to %s", len(l.spaces), len(l.slots), *dur, l.gw)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range *writers {
		wg.Go(func() { l.booker(stop) })
	}
	for range *readers {
		wg.Go(func() { l.reader(stop) })
	}
	wg.Go(func() { l.host(stop, *photoEvery) })
	tick := time.NewTicker(10 * time.Second)
	end := time.After(*dur)
loop:
	for {
		select {
		case <-tick.C:
			log.Print(l.counts())
		case <-end:
			break loop
		}
	}
	close(stop)
	wg.Wait()

	same, served, missing := l.checkPhotos()
	l.mu.Lock()
	defer l.mu.Unlock()
	writeJSON(*out+"-bookings.json", l.ids)
	writeJSON(*out+"-photos.json", l.photos)
	fmt.Printf("RESULT %s conflict=%d\n", l.counts(), l.conflict.Load())
	fmt.Printf("RESULT longest gap between bookings %s (from %s), slowest booking %s\n",
		l.maxGap.Round(time.Millisecond), l.gapAt.In(kst).Format("15:04:05.000"), l.slowest.Round(time.Millisecond))
	fmt.Printf("RESULT reads by backend %v\n", l.byBackend)
	fmt.Printf("RESULT photo recheck: %d of %d uploaded photos served now with identical bytes (SHA-256) (%d served, %d missing)\n", same, len(l.photos), served, missing)
}

func writeJSON(path string, v any) {
	b, _ := json.Marshal(v)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		log.Print(err)
	}
}

func (l *load) loadSpaces() error {
	var body struct {
		Items []space `json:"items"`
	}
	code, b, _, err := l.do(http.MethodGet, "/api/spaces?limit=100", "", nil)
	if err != nil || code != http.StatusOK || json.Unmarshal(b, &body) != nil || len(body.Items) == 0 {
		return fmt.Errorf("list spaces: status %d, err %v", code, err)
	}
	l.spaces = body.Items
	today := time.Now().In(kst)
	// The last days of the booking window, away from real users' near-term slots.
	for day := 45; day <= 59; day++ {
		date := today.AddDate(0, 0, day).Format(time.DateOnly)
		for _, s := range l.spaces {
			for h := s.OpenHour; h < s.CloseHour; h++ {
				l.slots = append(l.slots, slot{s.ID, date, h})
			}
		}
	}
	rand.Shuffle(len(l.slots), func(i, j int) { l.slots[i], l.slots[j] = l.slots[j], l.slots[i] })
	return nil
}

const pace = 100 * time.Millisecond

func (l *load) booker(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		i := l.next.Add(1) - 1
		if int(i) >= len(l.slots) {
			return
		}
		s := l.slots[i]
		body := fmt.Sprintf(`{"spaceId":%d,"date":%q,"startHour":%d,"endHour":%d,"headcount":1,"guestName":%q,"phone":"010-0000-0000"}`,
			s.space, s.date, s.hour, s.hour+1, guest)
		t0 := time.Now()
		code, b, _, err := l.do(http.MethodPost, "/api/reservations", body, nil)
		var r struct {
			ID int64 `json:"id"`
		}
		switch {
		case err == nil && code == http.StatusCreated && json.Unmarshal(b, &r) == nil && r.ID > 0:
			l.booked(r.ID, t0, time.Now())
		case err == nil && code == http.StatusConflict:
			l.conflict.Add(1)
		default:
			l.writeFail.Add(1)
			l.logFail("booking", code, err, b)
		}
		time.Sleep(pace)
	}
}

func (l *load) host(stop <-chan struct{}, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	base := int(time.Now().Unix())
	for n := 1; ; n++ {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		sp := l.spaces[rand.IntN(len(l.spaces))].ID
		size, sum, ctype, body := pngForm(base + n)
		code, b, _, err := l.do(http.MethodPost, fmt.Sprintf("/api/host/spaces/%d/photos", sp), body, map[string]string{
			"Content-Type": ctype, "X-Host-Code": l.hostCode,
		})
		var p struct {
			ID int64 `json:"id"`
		}
		if err == nil && code == http.StatusCreated && json.Unmarshal(b, &p) == nil && p.ID > 0 {
			l.photoOK.Add(1)
			l.mu.Lock()
			l.photos = append(l.photos, photo{Space: sp, ID: p.ID, Size: size, SHA: sum})
			l.mu.Unlock()
		} else {
			l.photoFail.Add(1)
			l.logFail("photo upload", code, err, b)
		}
	}
}

func pngForm(n int) (int, string, string, string) {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := range 64 {
		img.Set(i%8, i/8, color.RGBA{uint8(n), uint8(n >> 8), uint8(n >> 16), 255})
	}
	img.Set(0, 0, color.RGBA{uint8(n >> 24), 1, 2, 255})
	var pb bytes.Buffer
	_ = png.Encode(&pb, img)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, _ := w.CreateFormFile("photo", fmt.Sprintf("rehearsal-%d.png", n))
	_, _ = fw.Write(pb.Bytes())
	_ = w.Close()
	h := sha256.Sum256(pb.Bytes())
	return pb.Len(), hex.EncodeToString(h[:]), w.FormDataContentType(), body.String()
}

func (l *load) reader(stop <-chan struct{}) {
	for n := 0; ; n++ {
		select {
		case <-stop:
			return
		default:
		}
		path := fmt.Sprintf("/api/spaces/%d", l.spaces[rand.IntN(len(l.spaces))].ID)
		if n%2 == 1 {
			l.mu.Lock()
			if len(l.photos) > 0 {
				p := l.photos[rand.IntN(len(l.photos))]
				path = fmt.Sprintf("/api/spaces/%d/photos/%d", p.Space, p.ID)
			}
			l.mu.Unlock()
		}
		code, b, backend, err := l.do(http.MethodGet, path, "", nil)
		switch {
		case err == nil && code == http.StatusOK:
			l.readOK.Add(1)
		case err == nil && code == http.StatusNotFound:
			l.readMissing.Add(1)
		default:
			l.readFail.Add(1)
			l.logFail("read "+path, code, err, b)
		}
		if backend != "" {
			l.mu.Lock()
			l.byBackend[backend]++
			l.mu.Unlock()
		}
		time.Sleep(pace)
	}
}

func (l *load) checkPhotos() (same, served, missing int) {
	l.mu.Lock()
	ps := append([]photo(nil), l.photos...)
	l.mu.Unlock()
	for _, p := range ps {
		code, b, _, err := l.do(http.MethodGet, fmt.Sprintf("/api/spaces/%d/photos/%d", p.Space, p.ID), "", nil)
		switch {
		case err == nil && code == http.StatusOK:
			served++
			if h := sha256.Sum256(b); hex.EncodeToString(h[:]) == p.SHA {
				same++
			}
		case err == nil && code == http.StatusNotFound:
			missing++
		}
	}
	return
}

func (l *load) do(method, path, body string, hdr map[string]string) (int, []byte, string, error) {
	req, err := http.NewRequest(method, l.gw+path, strings.NewReader(body))
	if err != nil {
		return 0, nil, "", err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := l.hc.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, b, resp.Header.Get("X-Charlie-Backend"), nil
}

func (l *load) booked(id int64, t0, now time.Time) {
	l.writeOK.Add(1)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ids = append(l.ids, id)
	if !l.lastOK.IsZero() && now.Sub(l.lastOK) > l.maxGap {
		l.maxGap, l.gapAt = now.Sub(l.lastOK), l.lastOK
	}
	if now.After(l.lastOK) {
		l.lastOK = now
	}
	l.slowest = max(l.slowest, now.Sub(t0))
}

func (l *load) logFail(kind string, code int, err error, body []byte) {
	l.mu.Lock()
	l.failLogs++
	n := l.failLogs
	l.mu.Unlock()
	if n <= 20 {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 160 {
			msg = msg[:160]
		}
		log.Printf("   %s failed: status=%d err=%v %s", kind, code, err, msg)
	}
}

func (l *load) counts() string {
	return fmt.Sprintf("bookings ok=%d failed=%d, photos ok=%d failed=%d, reads ok=%d notFound=%d failed=%d",
		l.writeOK.Load(), l.writeFail.Load(), l.photoOK.Load(), l.photoFail.Load(), l.readOK.Load(), l.readMissing.Load(), l.readFail.Load())
}
