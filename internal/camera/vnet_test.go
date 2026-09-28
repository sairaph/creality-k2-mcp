package camera

// This package's WebRTC tests (session_test.go, snapshot_test.go,
// broadcaster_test.go, fakeprinter_test.go) never open a real network
// socket for the pion-to-pion (Session <-> fake printer) traffic: both
// sides run over pion's virtual network (pion/transport's vnet), an
// entirely in-process simulation with no real OS sockets at all. A real
// UDP socket, even one this package restricted to a single loopback
// address via SettingEngine.SetIPFilter, still made a freshly built test
// binary trigger a Windows Firewall prompt, since Windows treats every new,
// unsigned build as an unrecognized application regardless of which
// address it binds to. vnet removes the possibility entirely: there is no
// real socket for Firewall to ever notice.
//
// The only real network I/O left anywhere in this package's tests is the
// signaling HTTP round trip, which goes through httptest.NewServer -
// Go's own stdlib, which binds "127.0.0.1:0" (loopback only, verified
// against GOROOT's net/http/httptest/server.go), never ":0" or "0.0.0.0".
//
// TestMain below installs the vnet-backed newSessionAPI override before any
// test runs, so Session.Open (production code, otherwise unmodified) uses
// vnet for the whole test binary's lifetime. fakeprinter_test.go's HTTP
// handler builds its own PeerConnection directly and uses the printer side
// of the same virtual network.

import (
	"fmt"
	"os"
	"testing"

	"github.com/pion/ice/v4"
	"github.com/pion/logging"
	"github.com/pion/transport/v5/vnet"
	"github.com/pion/webrtc/v4"
)

// testClientVNetIP and testPrinterVNetIP are the two virtual hosts on the
// in-process vnet WAN this package's tests share: one standing in for the
// machine running Session.Open, one for the fake printer. Neither is a
// real, routable, or loopback address; they only exist inside the vnet
// router below.
const (
	testClientVNetIP  = "10.11.12.1"
	testPrinterVNetIP = "10.11.12.2"
)

// testPrinterVNet is the fake printer's virtual network interface,
// populated by TestMain before any test runs and used by
// fakeprinter_test.go's HTTP handler to build its PeerConnection.
var testPrinterVNet *vnet.Net

// testWAN is the shared vnet.Router every test's traffic (client and fake
// printer alike) flows through. nack_recovery_test.go uses
// testWAN.AddChunkFilter to drop one RTP media packet on demand, proving the
// NACK-based recovery fixed in dev_docs/camera-keyframe-rca.md. This
// package's tests never run concurrently within one binary (none call
// t.Parallel), so a filter installed at the start of one test only ever
// observes that test's own traffic.
var testWAN *vnet.Router

// TestMain sets up one shared virtual network for the whole test binary
// (cheap to create once, and every test's PeerConnection gets its own
// randomly allocated virtual ports on it, exactly like a real network
// stack) and installs it as this package's newSessionAPI, so every call to
// Open in every test in this package runs over vnet instead of real
// sockets.
func TestMain(m *testing.M) {
	wan, err := vnet.NewRouter(&vnet.RouterConfig{
		CIDR:          "10.11.12.0/24",
		LoggerFactory: logging.NewDefaultLoggerFactory(),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "camera tests: create vnet router:", err)
		os.Exit(1)
	}

	clientVNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{testClientVNetIP}})
	if err != nil {
		fmt.Fprintln(os.Stderr, "camera tests: create client vnet:", err)
		os.Exit(1)
	}
	if err := wan.AddNet(clientVNet); err != nil {
		fmt.Fprintln(os.Stderr, "camera tests: attach client vnet to router:", err)
		os.Exit(1)
	}

	printerVNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{testPrinterVNetIP}})
	if err != nil {
		fmt.Fprintln(os.Stderr, "camera tests: create printer vnet:", err)
		os.Exit(1)
	}
	if err := wan.AddNet(printerVNet); err != nil {
		fmt.Fprintln(os.Stderr, "camera tests: attach printer vnet to router:", err)
		os.Exit(1)
	}

	if err := wan.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "camera tests: start vnet router:", err)
		os.Exit(1)
	}

	testPrinterVNet = printerVNet
	testWAN = wan

	newSessionAPI = func(host string) (*webrtc.API, error) {
		settingEngine := webrtc.SettingEngine{}
		settingEngine.SetNet(clientVNet)
		// pion's mDNS candidate gathering opens a REAL multicast UDP socket
		// (224.0.0.0:5353) regardless of SetNet, since vnet has no
		// multicast support; disable it so this package's tests never
		// touch a real socket at all, not even one that immediately fails
		// to bind.
		settingEngine.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)

		// newMediaEngineWithNACK (session.go) is the same call production's
		// own newSessionAPI makes, so every test in this package exercises
		// the real NACK generator configuration rather than a
		// hand-duplicated approximation of it (dev_docs/camera-keyframe-
		// rca.md's corrected fix).
		mediaEngine, interceptorRegistry, err := newMediaEngineWithNACK()
		if err != nil {
			return nil, err
		}
		return webrtc.NewAPI(
			webrtc.WithMediaEngine(mediaEngine),
			webrtc.WithSettingEngine(settingEngine),
			webrtc.WithInterceptorRegistry(interceptorRegistry),
		), nil
	}

	os.Exit(m.Run())
}
