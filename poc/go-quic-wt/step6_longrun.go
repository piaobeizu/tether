//go:build step6

// Step 6: 30-minute long-running WebTransport connection.
//
// Validates the §11.T HARD GATE long-connection requirement:
// webtransport-go client mode must survive 30 minutes with periodic
// heartbeats, no leaks, no spurious reconnects. This is the last
// step before §11.T risk is fully cleared (modulo step 4 migration
// which needs real network changes).
//
// Test design:
//   - Open WT session, hold for 30 minutes
//   - Stream "control": ping every 5 sec (~360 pings total)
//   - Stream "events":  ping every 30 sec (~60 pings total)
//   - Datagram heartbeat: every 1 sec (~1800 datagrams)
//   - Print memory stats every 5 min (heap growth = leak signal)
//   - Track: total RTT distribution, errors, reconnects
//
// Note on "simulated packet loss": real loss simulation needs
// `tc qdisc add ... netem loss N%` on Linux (root). For PoC step 6
// we focus on long-run stability without artificial loss; loss
// resilience can be a separate v0.1.x test. Localhost loss is ~0
// so this validates clean-network long-connection only.
//
// Run:
//   ./bin-step1                      # terminal A (server)
//   ./bin-step6 > /tmp/step6.log &   # background
//   tail -f /tmp/step6.log
//
// PASS criteria:
//   - Wall-clock ≥ 30 min reached
//   - control pings sent ≈ 360, drops < 5
//   - events pings sent ≈ 60, drops < 1
//   - datagram sent ≈ 1800, recv ≥ 95% (datagrams are unreliable)
//   - heap growth < 50 MB over 30 min (no leak)
//   - 0 panics / 0 reconnects

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
)

const (
	server6URL    = "https://127.0.0.1:4433/wt"
	totalDuration = 30 * time.Minute
	ctrlInterval  = 5 * time.Second
	eventInterval = 30 * time.Second
	dgramInterval = 1 * time.Second
	memStatPeriod = 5 * time.Minute
	pingPayload   = "ping"
	maxRTTBuffer  = 4096
)

type pingStats struct {
	mu     sync.Mutex
	rtts   []time.Duration
	sent   int64
	failed int64
}

func (s *pingStats) record(rtt time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.rtts) >= maxRTTBuffer {
		// keep recent only
		copy(s.rtts, s.rtts[len(s.rtts)/2:])
		s.rtts = s.rtts[:len(s.rtts)/2]
	}
	s.rtts = append(s.rtts, rtt)
}

func (s *pingStats) snapshot() (sent, failed int64, p50, p99, max time.Duration) {
	s.mu.Lock()
	rtts := make([]time.Duration, len(s.rtts))
	copy(rtts, s.rtts)
	s.mu.Unlock()
	sent = atomic.LoadInt64(&s.sent)
	failed = atomic.LoadInt64(&s.failed)
	if len(rtts) == 0 {
		return
	}
	sort.Slice(rtts, func(i, j int) bool { return rtts[i] < rtts[j] })
	p50 = rtts[len(rtts)/2]
	p99 = rtts[(len(rtts)-1)*99/100]
	max = rtts[len(rtts)-1]
	return
}

func main() {
	d := &webtransport.Dialer{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"h3"},
		},
		QUICConfig: &quic.Config{
			MaxIncomingStreams:               256,
			MaxIncomingUniStreams:            256,
			EnableDatagrams:                  true,
			EnableStreamResetPartialDelivery: true,
			KeepAlivePeriod:                  15 * time.Second,
			HandshakeIdleTimeout:             10 * time.Second,
			MaxIdleTimeout:                   60 * time.Second,
		},
	}

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dialCancel()
	rsp, sess, err := d.Dial(dialCtx, server6URL, nil)
	if err != nil {
		fail6("dial: %v", err)
	}
	if rsp.StatusCode != 200 {
		fail6("status: %d", rsp.StatusCode)
	}
	fmt.Printf("[%s] → session opened (remote=%s)\n", ts(), sess.RemoteAddr())

	ctx, cancel := context.WithTimeout(context.Background(), totalDuration+1*time.Minute)
	defer cancel()

	startMem := readHeapMB()
	fmt.Printf("[%s] start: heap=%dMB\n", ts(), startMem)

	ctrlStats := &pingStats{}
	eventStats := &pingStats{}
	var dgramSent, dgramRecv int64

	var wg sync.WaitGroup

	// stream pings
	for _, spec := range []struct {
		name     string
		interval time.Duration
		stats    *pingStats
	}{
		{"control", ctrlInterval, ctrlStats},
		{"events", eventInterval, eventStats},
	} {
		wg.Add(1)
		go func(name string, interval time.Duration, stats *pingStats) {
			defer wg.Done()
			runPersistentPing(ctx, sess, name, interval, stats)
		}(spec.name, spec.interval, spec.stats)
	}

	// datagram heartbeat
	wg.Add(1)
	go func() {
		defer wg.Done()
		runDatagramHB(ctx, sess, &dgramSent, &dgramRecv)
	}()

	// periodic memory + progress reporter
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(memStatPeriod)
		defer ticker.Stop()
		started := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				heap := readHeapMB()
				cs, cf, cp50, cp99, cmax := ctrlStats.snapshot()
				es, ef, ep50, ep99, emax := eventStats.snapshot()
				ds := atomic.LoadInt64(&dgramSent)
				dr := atomic.LoadInt64(&dgramRecv)
				elapsed := time.Since(started).Round(time.Second)
				fmt.Printf("[%s] +%v heap=%dMB ctrl(s=%d f=%d p50=%v p99=%v max=%v) evt(s=%d f=%d p50=%v p99=%v max=%v) dgram(s=%d r=%d %.1f%%)\n",
					ts(), elapsed, heap,
					cs, cf, cp50, cp99, cmax,
					es, ef, ep50, ep99, emax,
					ds, dr, 100*float64(dr)/float64(max1(ds)),
				)
			}
		}
	}()

	// duration timer
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-time.After(totalDuration):
			fmt.Printf("[%s] 30 min reached, stopping\n", ts())
			cancel()
		case <-ctx.Done():
		}
	}()

	wg.Wait()

	endMem := readHeapMB()
	heapGrowth := endMem - startMem
	fmt.Printf("[%s] end: heap=%dMB growth=%dMB\n", ts(), endMem, heapGrowth)

	cs, cf, cp50, cp99, cmax := ctrlStats.snapshot()
	es, ef, ep50, ep99, emax := eventStats.snapshot()
	ds := atomic.LoadInt64(&dgramSent)
	dr := atomic.LoadInt64(&dgramRecv)

	fmt.Println("\n=== Final stats ===")
	fmt.Printf("  control  sent=%d failed=%d p50=%v p99=%v max=%v\n", cs, cf, cp50, cp99, cmax)
	fmt.Printf("  events   sent=%d failed=%d p50=%v p99=%v max=%v\n", es, ef, ep50, ep99, emax)
	fmt.Printf("  datagram sent=%d recv=%d (%.1f%%)\n", ds, dr, 100*float64(dr)/float64(max1(ds)))
	fmt.Printf("  heap     growth=%dMB\n", heapGrowth)

	// pass criteria
	if cs < 350 {
		fail6("control sent=%d < 350", cs)
	}
	if cf > 5 {
		fail6("control failed=%d > 5", cf)
	}
	if es < 55 {
		fail6("events sent=%d < 55", es)
	}
	if ef > 1 {
		fail6("events failed=%d > 1", ef)
	}
	if ds < 1700 {
		fail6("datagram sent=%d < 1700", ds)
	}
	dgramLoss := float64(ds-dr) / float64(max1(ds))
	if dgramLoss > 0.05 {
		fail6("datagram loss=%.1f%% > 5%%", dgramLoss*100)
	}
	if heapGrowth > 50 {
		fail6("heap growth=%dMB > 50MB (leak suspected)", heapGrowth)
	}

	if err := sess.CloseWithError(0, "bye"); err != nil {
		fmt.Fprintf(os.Stderr, "close: %v\n", err)
	}
	fmt.Println("\n✓ STEP 6 PASS")
}

func runPersistentPing(ctx context.Context, sess *webtransport.Session, name string, interval time.Duration, stats *pingStats) {
	str, err := sess.OpenStreamSync(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] %s open: %v\n", ts(), name, err)
		return
	}
	defer str.Close()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	rdBuf := make([]byte, len(pingPayload))
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		atomic.AddInt64(&stats.sent, 1)
		t0 := time.Now()
		if _, err := str.Write([]byte(pingPayload)); err != nil {
			atomic.AddInt64(&stats.failed, 1)
			fmt.Fprintf(os.Stderr, "[%s] %s write: %v\n", ts(), name, err)
			return
		}
		// read echo
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		done := make(chan error, 1)
		go func() {
			_, err := io.ReadFull(str, rdBuf)
			done <- err
		}()
		select {
		case err := <-done:
			cancel()
			if err != nil {
				atomic.AddInt64(&stats.failed, 1)
				fmt.Fprintf(os.Stderr, "[%s] %s read: %v\n", ts(), name, err)
				return
			}
		case <-readCtx.Done():
			cancel()
			atomic.AddInt64(&stats.failed, 1)
			fmt.Fprintf(os.Stderr, "[%s] %s read timeout\n", ts(), name)
			return
		}
		if !bytes.Equal(rdBuf, []byte(pingPayload)) {
			atomic.AddInt64(&stats.failed, 1)
			return
		}
		stats.record(time.Since(t0))
	}
}

func runDatagramHB(ctx context.Context, sess *webtransport.Session, sent, recv *int64) {
	// receiver
	go func() {
		for {
			_, err := sess.ReceiveDatagram(ctx)
			if err != nil {
				return
			}
			atomic.AddInt64(recv, 1)
		}
	}()
	// sender
	ticker := time.NewTicker(dgramInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		seq := atomic.LoadInt64(sent)
		payload := []byte(fmt.Sprintf("hb-%06d", seq))
		if err := sess.SendDatagram(payload); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] dgram send: %v\n", ts(), err)
			return
		}
		atomic.AddInt64(sent, 1)
	}
}

func readHeapMB() int64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc / 1024 / 1024)
}

func ts() string {
	return time.Now().Format("15:04:05")
}

func max1(x int64) int64 {
	if x < 1 {
		return 1
	}
	return x
}

func fail6(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "\n✗ STEP 6 FAIL: "+format+"\n", a...)
	os.Exit(1)
}
