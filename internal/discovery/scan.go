package discovery

import (
	"context"
	"net"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/crealityws"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

// Dialer opens a TCP connection, matching (*net.Dialer).DialContext's
// signature so tests can inject a fake that never touches a real socket.
type Dialer func(ctx context.Context, network, address string) (net.Conn, error)

// Progress reports how a scan is advancing, so a caller (the wizard step's
// spinner, the TUI) can show live counts (plan-v0.1.0.md decision 4).
type Progress struct {
	Scanned int
	Total   int
	Found   int
}

// ProgressFunc is called as hosts finish their dial check, from whichever
// scan worker goroutine finished. It may be called concurrently from
// multiple goroutines; a caller that touches shared state (a UI model, a
// counter) must synchronize itself, for example by sending on a channel
// rather than mutating state directly.
type ProgressFunc func(Progress)

// ScanOptions configures Scan. The zero value uses the plan-v0.1.0.md
// decision 3 defaults for every field except Dialer (a real *net.Dialer)
// and Progress (no reporting).
type ScanOptions struct {
	// Port is the Moonraker port to try. Default domain.DefaultMoonrakerPort
	// (7125).
	Port int
	// WSPort is the port 9999 client's port for K2 confirmation. Default
	// crealityws.DefaultPort (9999). Exposed so tests can point it at a
	// fake server on another port.
	WSPort int
	// DialTimeout bounds each TCP connect attempt. Default 300ms.
	DialTimeout time.Duration
	// MaxConcurrency bounds how many dials are in flight at once. Default
	// 128.
	MaxConcurrency int
	// Budget bounds the whole scan, across every host, regardless of how
	// many are left to try. Default 15s.
	Budget time.Duration
	// Dialer opens the TCP connect check. Default a real net.Dialer.
	Dialer Dialer
	// Progress, if set, is called after every host finishes its dial
	// check (see ProgressFunc).
	Progress ProgressFunc
}

func (o ScanOptions) withDefaults() ScanOptions {
	if o.Port == 0 {
		o.Port = domain.DefaultMoonrakerPort
	}
	if o.WSPort == 0 {
		o.WSPort = crealityws.DefaultPort
	}
	if o.DialTimeout == 0 {
		o.DialTimeout = 300 * time.Millisecond
	}
	if o.MaxConcurrency <= 0 {
		o.MaxConcurrency = 128
	}
	if o.Budget == 0 {
		o.Budget = 15 * time.Second
	}
	if o.Dialer == nil {
		var d net.Dialer
		o.Dialer = d.DialContext
	}
	return o
}

// Report is a scan's outcome.
type Report struct {
	// Results holds one entry per host that answered the Moonraker port,
	// in host order.
	Results []Result
	// Partial is true when the budget elapsed, or ctx was cancelled,
	// before every host in the target list was tried. Results still holds
	// everything found before that point.
	Partial bool
	// Scanned is how many hosts actually got a dial attempt.
	Scanned int
	// Total is how many hosts were targeted.
	Total int
}

// Scan tries every host in hosts for an open Moonraker port, then
// identifies each responder (plan-v0.1.0.md decision 3): at most
// opts.MaxConcurrency dials in flight at once, each bounded by
// opts.DialTimeout, the whole scan bounded by opts.Budget. A caller can
// also cancel ctx early (for example the wizard step's esc key to stop a
// running scan). Either way Scan returns promptly with whatever was found
// so far, Partial set to true.
func Scan(ctx context.Context, hosts []string, opts ScanOptions) Report {
	opts = opts.withDefaults()

	budgetCtx, cancel := context.WithTimeout(ctx, opts.Budget)
	defer cancel()

	total := len(hosts)
	sem := make(chan struct{}, opts.MaxConcurrency)

	var (
		mu      sync.Mutex
		results []Result
		scanned int32
		wg      sync.WaitGroup
	)

	launched := 0
	report := func() {
		if opts.Progress == nil {
			return
		}
		mu.Lock()
		found := len(results)
		mu.Unlock()
		opts.Progress(Progress{
			Scanned: int(atomic.LoadInt32(&scanned)),
			Total:   total,
			Found:   found,
		})
	}

hostLoop:
	for _, host := range hosts {
		select {
		case <-budgetCtx.Done():
			break hostLoop
		case sem <- struct{}{}:
		}
		launched++
		host := host
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			if dialCheck(budgetCtx, opts.Dialer, host, opts.Port, opts.DialTimeout) {
				res := identify(budgetCtx, host, opts.Port, opts.WSPort)
				mu.Lock()
				results = append(results, res)
				mu.Unlock()
			}
			atomic.AddInt32(&scanned, 1)
			report()
		}()
	}
	wg.Wait()

	sort.Slice(results, func(i, j int) bool { return results[i].Host < results[j].Host })

	partial := launched < total || budgetCtx.Err() != nil
	return Report{
		Results: results,
		Partial: partial,
		Scanned: int(scanned),
		Total:   total,
	}
}

// dialCheck reports whether a TCP connect to host:port succeeds within
// timeout (bounded further by ctx, whichever is sooner).
func dialCheck(ctx context.Context, dial Dialer, host string, port int, timeout time.Duration) bool {
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := dial(dialCtx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Options configures Discover: interface/subnet enumeration plus everything
// ScanOptions controls for the scan that follows.
type Options struct {
	// Interfaces supplies this machine's network interfaces. A nil value
	// defaults to SystemInterfaces.
	Interfaces InterfaceSource
	ScanOptions
}

// Discover enumerates this machine's LAN subnets (Subnets/TargetHosts) and
// scans them (Scan). This is the function the wizard step, the unattended
// install path, the TUI, "printers scan" and discover_printers all share
// (plan-v0.1.0.md decision 3).
func Discover(ctx context.Context, opts Options) (Report, error) {
	hosts, err := TargetHosts(opts.Interfaces)
	if err != nil {
		return Report{}, err
	}
	return Scan(ctx, hosts, opts.ScanOptions), nil
}
