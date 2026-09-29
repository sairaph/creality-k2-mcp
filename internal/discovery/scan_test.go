package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// alwaysFailDialer never touches a real socket; every dial fails
// immediately.
func alwaysFailDialer(ctx context.Context, network, address string) (net.Conn, error) {
	return nil, errors.New("fake dialer: refused")
}

// blockingDialer blocks until ctx is done and then reports failure, standing
// in for a host that never answers (the common case on a real LAN scan).
func blockingDialer(ctx context.Context, network, address string) (net.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func genHosts(n int) []string {
	hosts := make([]string, n)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("10.0.%d.%d", i/250, i%250+1)
	}
	return hosts
}

func TestScanAllUnreachableProducesNoResults(t *testing.T) {
	hosts := genHosts(20)
	rep := Scan(context.Background(), hosts, ScanOptions{
		Dialer:      alwaysFailDialer,
		DialTimeout: 50 * time.Millisecond,
		Budget:      5 * time.Second,
	})
	if len(rep.Results) != 0 {
		t.Fatalf("Results = %v, want none", rep.Results)
	}
	if rep.Scanned != len(hosts) {
		t.Fatalf("Scanned = %d, want %d", rep.Scanned, len(hosts))
	}
	if rep.Total != len(hosts) {
		t.Fatalf("Total = %d, want %d", rep.Total, len(hosts))
	}
	if rep.Partial {
		t.Fatal("Partial = true, want false: every host was tried within budget")
	}
}

func TestScanBudgetElapsedReportsPartial(t *testing.T) {
	hosts := genHosts(500)
	start := time.Now()
	rep := Scan(context.Background(), hosts, ScanOptions{
		Dialer:         blockingDialer,
		DialTimeout:    2 * time.Second, // longer than the budget below
		MaxConcurrency: 4,
		Budget:         80 * time.Millisecond,
	})
	elapsed := time.Since(start)

	if !rep.Partial {
		t.Fatalf("Partial = false, want true: budget could not possibly cover %d hosts at concurrency 4", len(hosts))
	}
	if rep.Scanned >= rep.Total {
		t.Fatalf("Scanned = %d, Total = %d: expected the budget to cut the scan short", rep.Scanned, rep.Total)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Scan took %s, want well under its dial timeout: the budget must cut it short, not the dial timeout", elapsed)
	}
}

func TestScanContextCancelledReportsPartial(t *testing.T) {
	hosts := genHosts(500)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)

	start := time.Now()
	rep := Scan(ctx, hosts, ScanOptions{
		Dialer:         blockingDialer,
		DialTimeout:    2 * time.Second,
		MaxConcurrency: 4,
		Budget:         15 * time.Second, // budget alone would not stop this
	})
	elapsed := time.Since(start)

	if !rep.Partial {
		t.Fatal("Partial = false, want true: the context was cancelled early")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Scan took %s after context cancellation, want it to return promptly", elapsed)
	}
}

func TestScanRespectsMaxConcurrency(t *testing.T) {
	const maxConcurrency = 5
	var (
		current int32
		peak    int32
	)
	dialer := func(ctx context.Context, network, address string) (net.Conn, error) {
		n := atomic.AddInt32(&current, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		atomic.AddInt32(&current, -1)
		return nil, errors.New("fake dialer: refused")
	}

	hosts := genHosts(60)
	rep := Scan(context.Background(), hosts, ScanOptions{
		Dialer:         dialer,
		DialTimeout:    time.Second,
		MaxConcurrency: maxConcurrency,
		Budget:         10 * time.Second,
	})

	if rep.Scanned != len(hosts) {
		t.Fatalf("Scanned = %d, want %d (budget was generous)", rep.Scanned, len(hosts))
	}
	if got := atomic.LoadInt32(&peak); got > maxConcurrency {
		t.Fatalf("peak concurrent dials = %d, want at most %d", got, maxConcurrency)
	}
}

func TestScanFindsAndIdentifiesRealListener(t *testing.T) {
	moonrakerHost, moonrakerPort := startFakeMoonraker(t, "K2-5885", "1.5.0")
	frame := pushFrame(t, idleK2PushFields(t), nil)
	_, wsPort := startFakeWSServer(t, [][]byte{frame})

	// A real (default) dialer against a real local listener: loopback
	// traffic only, never the LAN.
	rep := Scan(context.Background(), []string{moonrakerHost}, ScanOptions{
		Port:        moonrakerPort,
		WSPort:      wsPort,
		DialTimeout: time.Second,
		Budget:      10 * time.Second,
	})

	if rep.Partial {
		t.Fatal("Partial = true, want false")
	}
	if len(rep.Results) != 1 {
		t.Fatalf("Results = %v, want exactly 1", rep.Results)
	}
	res := rep.Results[0]
	if !res.IdentifiedK2 {
		t.Fatalf("IdentifiedK2 = false, want true; res = %+v", res)
	}
	if res.Model != "F021" {
		t.Fatalf("Model = %q, want F021", res.Model)
	}
}

func TestScanProgressReportsMonotonicScannedCount(t *testing.T) {
	hosts := genHosts(30)
	var mu sync.Mutex
	var lastScanned int
	var calls int

	rep := Scan(context.Background(), hosts, ScanOptions{
		Dialer:         alwaysFailDialer,
		DialTimeout:    50 * time.Millisecond,
		MaxConcurrency: 8,
		Budget:         5 * time.Second,
		Progress: func(p Progress) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if p.Scanned < lastScanned {
				t.Errorf("Progress.Scanned went backwards: %d then %d", lastScanned, p.Scanned)
			}
			// Calls are serialized and in order: every call is exactly one more.
			if p.Scanned != lastScanned+1 {
				t.Errorf("Progress.Scanned = %d after %d, want the next count", p.Scanned, lastScanned)
			}
			lastScanned = p.Scanned
			if p.Total != len(hosts) {
				t.Errorf("Progress.Total = %d, want %d", p.Total, len(hosts))
			}
		},
	})

	if calls != len(hosts) {
		t.Fatalf("Progress was called %d times, want %d (once per host)", calls, len(hosts))
	}
	if lastScanned != rep.Scanned || lastScanned != len(hosts) {
		t.Fatalf("final Progress.Scanned = %d, want %d (report %d)", lastScanned, len(hosts), rep.Scanned)
	}
}

func TestDiscoverUsesInterfaceSourceAndScanOptions(t *testing.T) {
	source := fakeSource(InterfaceInfo{
		Name: "Ethernet", Up: true,
		Addrs: []net.Addr{ipNet("10.0.0.5/24")},
	})

	rep, err := Discover(context.Background(), Options{
		Interfaces: source,
		ScanOptions: ScanOptions{
			Dialer:         alwaysFailDialer,
			DialTimeout:    20 * time.Millisecond,
			MaxConcurrency: 32,
			Budget:         5 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if rep.Total != 254 {
		t.Fatalf("Total = %d, want 254 (a /24 has 254 usable hosts)", rep.Total)
	}
	if rep.Scanned != 254 {
		t.Fatalf("Scanned = %d, want 254", rep.Scanned)
	}
	if len(rep.Results) != 0 {
		t.Fatalf("Results = %v, want none (fake dialer never succeeds)", rep.Results)
	}
}

func TestDiscoverPropagatesInterfaceSourceError(t *testing.T) {
	wantErr := errors.New("boom")
	_, err := Discover(context.Background(), Options{
		Interfaces: func() ([]InterfaceInfo, error) { return nil, wantErr },
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}
