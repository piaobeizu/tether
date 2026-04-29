//go:build step5

// Step 5: multi-stream parallel + datagram heartbeat.
//
// Validates the §11.T HARD GATE core requirement: the 5-channel wire
// model (D-14) actually works on webtransport-go v0.10. Open 4 bidi
// streams in parallel + datagram heartbeat, send concurrently, measure
// per-stream behavior.
//
// Streams (mapped to §3.3.3):
//   stream 1 (control): 100x small msgs (32B), measure RTT distribution
//   stream 2 (events) : 100x small msgs (32B), measure RTT distribution
//   stream 3 (a-bytes): one 5MB blob, measure throughput + integrity
//   stream 4 (catchup): 100x small msgs (32B) (in PoC just bidi echo,
//                       real spec is server-only unidirectional)
//
// Datagram: 5Hz heartbeat for ~6s (30 datagrams). Verify echo count.
//
// Run:
//   ./bin-step1                # terminal A
//   ./bin-step5                # terminal B
//
// PASS criteria:
//   - All 4 streams complete with byte-level integrity
//   - Datagram round-trip count >= 28/30 (allow 2 loss for unreliable channel)
//   - cc-bytes throughput > 1 Gbps on localhost (sanity, not a production target)
//   - Control/events RTT p99 < 50ms on localhost while cc-bytes uploading
//   - Total wall-clock < 30s

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
)

const (
	serverURL5    = "https://127.0.0.1:4433/wt"
	smallMsgCount = 100
	smallMsgSize  = 32
	bigBlobSize   = 5 * 1024 * 1024 // 5MB
	dgramRateHz   = 5
	dgramDuration = 6 * time.Second
)

type streamStats struct {
	name      string
	rtts      []time.Duration
	bytes     int64
	durationS float64
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
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rsp, sess, err := d.Dial(ctx, serverURL5, nil)
	if err != nil {
		fail5("dial: %v", err)
	}
	if rsp.StatusCode != 200 {
		fail5("status: %d", rsp.StatusCode)
	}
	fmt.Printf("→ session opened (remote=%s)\n", sess.RemoteAddr())

	results := make(chan *streamStats, 4)
	dgramResult := make(chan struct {
		sent, recv int
	}, 1)

	wallStart := time.Now()
	var wg sync.WaitGroup

	// 4 bidi streams (control, events, a-bytes, catchup) all in parallel
	for _, spec := range []struct {
		name string
		big  bool
	}{
		{"control", false},
		{"events", false},
		{"a-bytes", true},
		{"catchup", false},
	} {
		wg.Add(1)
		go func(name string, big bool) {
			defer wg.Done()
			stats, err := runStream(ctx, sess, name, big)
			if err != nil {
				fail5("stream %q: %v", name, err)
			}
			results <- stats
		}(spec.name, spec.big)
	}

	// datagram heartbeat
	wg.Add(1)
	go func() {
		defer wg.Done()
		sent, recv := runDatagrams(ctx, sess)
		dgramResult <- struct{ sent, recv int }{sent, recv}
	}()

	wg.Wait()
	close(results)
	close(dgramResult)
	wallElapsed := time.Since(wallStart)

	// collect stats
	statsByName := map[string]*streamStats{}
	for s := range results {
		statsByName[s.name] = s
	}
	dr := <-dgramResult

	// report
	fmt.Println("\n=== Per-stream stats ===")
	for _, name := range []string{"control", "events", "a-bytes", "catchup"} {
		s := statsByName[name]
		if s == nil {
			fail5("missing stats for %q", name)
		}
		if name == "a-bytes" {
			thrMbps := float64(s.bytes*8) / s.durationS / 1e6
			fmt.Printf("  %-8s  bytes=%d  duration=%.2fs  throughput=%.1f Mbps\n",
				s.name, s.bytes, s.durationS, thrMbps)
		} else {
			p50, p99 := percentile(s.rtts, 0.50), percentile(s.rtts, 0.99)
			fmt.Printf("  %-8s  msgs=%d  p50=%v  p99=%v\n",
				s.name, len(s.rtts), p50, p99)
		}
	}
	fmt.Printf("\n=== Datagram ===\n")
	fmt.Printf("  sent=%d  recv=%d  loss=%.1f%%\n",
		dr.sent, dr.recv, 100*float64(dr.sent-dr.recv)/float64(dr.sent))
	fmt.Printf("\n=== Wall ===\n  total=%v\n", wallElapsed)

	// pass criteria
	for _, name := range []string{"control", "events", "catchup"} {
		s := statsByName[name]
		p99 := percentile(s.rtts, 0.99)
		if p99 > 50*time.Millisecond {
			fail5("%q p99=%v > 50ms (HOL blocking? slow loopback?)", name, p99)
		}
	}
	if dr.recv < 28 {
		fail5("datagram recv=%d < 28/30 threshold", dr.recv)
	}
	abytes := statsByName["a-bytes"]
	thrMbps := float64(abytes.bytes*8) / abytes.durationS / 1e6
	if thrMbps < 1000 {
		fail5("a-bytes throughput=%.1f Mbps < 1000 Mbps localhost sanity", thrMbps)
	}
	if wallElapsed > 30*time.Second {
		fail5("wall-clock %v > 30s budget", wallElapsed)
	}

	if err := sess.CloseWithError(0, "bye"); err != nil {
		fmt.Fprintf(os.Stderr, "close: %v\n", err)
	}
	fmt.Println("\n✓ STEP 5 PASS")
}

// runStream: open one bidi stream, send N small messages or one big blob,
// echo back, measure.
func runStream(ctx context.Context, sess *webtransport.Session, name string, big bool) (*streamStats, error) {
	str, err := sess.OpenStreamSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}

	stats := &streamStats{name: name}

	if big {
		// send 5MB blob, read all back, verify integrity.
		payload := make([]byte, bigBlobSize)
		if _, err := rand.Read(payload); err != nil {
			return nil, fmt.Errorf("rand: %w", err)
		}
		expectHash := sha256.Sum256(payload)

		t0 := time.Now()
		writeDoneCh := make(chan error, 1)
		go func() {
			_, werr := str.Write(payload)
			if cerr := str.Close(); cerr != nil && werr == nil {
				werr = cerr
			}
			writeDoneCh <- werr
		}()
		got, err := io.ReadAll(str)
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("read: %w", err)
		}
		if werr := <-writeDoneCh; werr != nil {
			return nil, fmt.Errorf("write: %w", werr)
		}
		dur := time.Since(t0)
		stats.bytes = int64(len(got))
		stats.durationS = dur.Seconds()

		if len(got) != bigBlobSize {
			return nil, fmt.Errorf("size mismatch: want %d got %d", bigBlobSize, len(got))
		}
		gotHash := sha256.Sum256(got)
		if gotHash != expectHash {
			return nil, fmt.Errorf("hash mismatch")
		}
		return stats, nil
	}

	// small messages: send/recv N times, measure RTT.
	stats.rtts = make([]time.Duration, 0, smallMsgCount)
	buf := make([]byte, smallMsgSize)
	rdBuf := make([]byte, smallMsgSize)
	for i := 0; i < smallMsgCount; i++ {
		// fill payload with sequence number for order verification
		fmt.Fprintf(bytes.NewBuffer(buf[:0]), "%s-msg-%04d", name, i)
		// pad to fixed size
		filled := []byte(fmt.Sprintf("%s-msg-%04d", name, i))
		copy(buf, filled)
		for k := len(filled); k < smallMsgSize; k++ {
			buf[k] = ' '
		}

		t0 := time.Now()
		if _, err := str.Write(buf); err != nil {
			return nil, fmt.Errorf("msg %d write: %w", i, err)
		}
		if _, err := io.ReadFull(str, rdBuf); err != nil {
			return nil, fmt.Errorf("msg %d read: %w", i, err)
		}
		stats.rtts = append(stats.rtts, time.Since(t0))

		if !bytes.Equal(buf, rdBuf) {
			return nil, fmt.Errorf("msg %d echo mismatch", i)
		}
	}
	if err := str.Close(); err != nil {
		return nil, fmt.Errorf("close: %w", err)
	}
	return stats, nil
}

// runDatagrams: send heartbeat datagrams at fixed rate, count echoes.
func runDatagrams(ctx context.Context, sess *webtransport.Session) (sent, recv int) {
	dgramCtx, cancel := context.WithTimeout(ctx, dgramDuration+2*time.Second)
	defer cancel()

	var sentN, recvN int64
	var wg sync.WaitGroup

	// receiver
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			_, err := sess.ReceiveDatagram(dgramCtx)
			if err != nil {
				return
			}
			atomic.AddInt64(&recvN, 1)
		}
	}()

	// sender
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(time.Second / time.Duration(dgramRateHz))
		defer ticker.Stop()
		deadline := time.Now().Add(dgramDuration)
		for time.Now().Before(deadline) {
			select {
			case <-ticker.C:
			case <-dgramCtx.Done():
				return
			}
			payload := []byte(fmt.Sprintf("hb-%04d", atomic.LoadInt64(&sentN)))
			if err := sess.SendDatagram(payload); err != nil {
				return
			}
			atomic.AddInt64(&sentN, 1)
		}
	}()

	// give receiver a brief drain after sender stops
	go func() {
		<-time.After(dgramDuration + 500*time.Millisecond)
		cancel()
	}()
	wg.Wait()
	return int(sentN), int(recvN)
}

func percentile(xs []time.Duration, p float64) time.Duration {
	if len(xs) == 0 {
		return 0
	}
	cp := make([]time.Duration, len(xs))
	copy(cp, xs)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(float64(len(cp)-1) * p)
	return cp[idx]
}

func fail5(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "\n✗ STEP 5 FAIL: "+format+"\n", a...)
	os.Exit(1)
}
