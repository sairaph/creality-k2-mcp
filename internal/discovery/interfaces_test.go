package discovery

import (
	"errors"
	"net"
	"testing"
)

func ipNet(cidr string) *net.IPNet {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	ipnet.IP = ip
	return ipnet
}

func fakeSource(ifaces ...InterfaceInfo) InterfaceSource {
	return func() ([]InterfaceInfo, error) { return ifaces, nil }
}

func TestSubnetsNarrowsLargeSubnetToSlash22(t *testing.T) {
	source := fakeSource(InterfaceInfo{
		Name: "Ethernet", Up: true,
		Addrs: []net.Addr{ipNet("10.0.0.5/16")},
	})
	subs, err := Subnets(source)
	if err != nil {
		t.Fatalf("Subnets: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("got %d subnets, want 1: %v", len(subs), subs)
	}
	if subs[0].Bits != 22 {
		t.Fatalf("Bits = %d, want 22", subs[0].Bits)
	}
	if subs[0].Network.String() != "10.0.0.0" {
		t.Fatalf("Network = %s, want 10.0.0.0", subs[0].Network)
	}
}

func TestSubnetsLeavesSmallSubnetAlone(t *testing.T) {
	source := fakeSource(InterfaceInfo{
		Name: "Ethernet", Up: true,
		Addrs: []net.Addr{ipNet("192.168.1.10/24")},
	})
	subs, err := Subnets(source)
	if err != nil {
		t.Fatalf("Subnets: %v", err)
	}
	if len(subs) != 1 || subs[0].Bits != 24 || subs[0].Network.String() != "192.168.1.0" {
		t.Fatalf("got %v, want one 192.168.1.0/24", subs)
	}
}

func TestSubnetsSkipsLoopback(t *testing.T) {
	source := fakeSource(InterfaceInfo{
		Name: "lo", Up: true,
		Addrs: []net.Addr{ipNet("127.0.0.1/8")},
	})
	subs, err := Subnets(source)
	if err != nil {
		t.Fatalf("Subnets: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("got %v, want none (loopback)", subs)
	}
}

func TestSubnetsSkipsLinkLocal(t *testing.T) {
	source := fakeSource(InterfaceInfo{
		Name: "Ethernet", Up: true,
		Addrs: []net.Addr{ipNet("169.254.1.5/16")},
	})
	subs, err := Subnets(source)
	if err != nil {
		t.Fatalf("Subnets: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("got %v, want none (link-local)", subs)
	}
}

func TestSubnetsSkipsDownInterface(t *testing.T) {
	source := fakeSource(InterfaceInfo{
		Name: "Ethernet", Up: false,
		Addrs: []net.Addr{ipNet("192.168.1.10/24")},
	})
	subs, err := Subnets(source)
	if err != nil {
		t.Fatalf("Subnets: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("got %v, want none (interface down)", subs)
	}
}

func TestSubnetsSkipsIPv6(t *testing.T) {
	source := fakeSource(InterfaceInfo{
		Name: "Ethernet", Up: true,
		Addrs: []net.Addr{ipNet("fe80::1/64")},
	})
	subs, err := Subnets(source)
	if err != nil {
		t.Fatalf("Subnets: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("got %v, want none (IPv6 out of scope)", subs)
	}
}

func TestSubnetsSkipsVirtualAdaptersByName(t *testing.T) {
	names := []string{
		"vEthernet (WSL)",
		"vEthernet (Default Switch)",
		"Hyper-V Virtual Ethernet Adapter",
		"docker0",
		"br-1234567890ab",
		"vboxnet0",
		"VirtualBox Host-Only Network",
		"VMware Network Adapter VMnet8",
		"tun0",
		"tap0",
		"utun3",
		"wg0",
		"Tailscale",
		"ZeroTier One",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			source := fakeSource(InterfaceInfo{
				Name: name, Up: true,
				Addrs: []net.Addr{ipNet("192.168.50.10/24")},
			})
			subs, err := Subnets(source)
			if err != nil {
				t.Fatalf("Subnets: %v", err)
			}
			if len(subs) != 0 {
				t.Fatalf("interface %q: got %v, want none (virtual adapter)", name, subs)
			}
		})
	}
}

func TestSubnetsDoesNotSkipRealSoundingNames(t *testing.T) {
	names := []string{"Ethernet", "Wi-Fi", "eth0", "en0", "Local Area Connection"}
	for _, name := range names {
		if isVirtualAdapter(name) {
			t.Fatalf("interface %q should not be classified virtual", name)
		}
	}
}

func TestSubnetsDedupesAcrossInterfaces(t *testing.T) {
	source := fakeSource(
		InterfaceInfo{Name: "Ethernet1", Up: true, Addrs: []net.Addr{ipNet("10.0.1.5/22")}},
		InterfaceInfo{Name: "Ethernet2", Up: true, Addrs: []net.Addr{ipNet("10.0.2.9/22")}},
	)
	subs, err := Subnets(source)
	if err != nil {
		t.Fatalf("Subnets: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("got %d subnets, want 1 (same /22 range): %v", len(subs), subs)
	}
}

func TestSubnetsErrorPropagates(t *testing.T) {
	wantErr := errors.New("boom")
	source := func() ([]InterfaceInfo, error) { return nil, wantErr }
	_, err := Subnets(source)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

func TestSubnetsNilSourceUsesSystemInterfaces(t *testing.T) {
	// Just verify it does not panic and returns without error on the real
	// machine; this is the only test allowed to touch the real interface
	// list, and it never dials anything.
	if _, err := Subnets(nil); err != nil {
		t.Fatalf("Subnets(nil): %v", err)
	}
}

func TestSubnetHostsExcludesNetworkAndBroadcast(t *testing.T) {
	s := Subnet{Network: net.ParseIP("10.0.0.0").To4(), Bits: 22}
	hosts := s.Hosts()
	if len(hosts) != 1022 {
		t.Fatalf("len(hosts) = %d, want 1022", len(hosts))
	}
	// Hosts() enumerates in ascending numeric order; check the endpoints
	// directly rather than through a lexicographic sort (which would put
	// "10.0.3.99" before "10.0.3.254").
	if hosts[0] != "10.0.0.1" {
		t.Fatalf("first host = %s, want 10.0.0.1", hosts[0])
	}
	last := hosts[len(hosts)-1]
	if last != "10.0.3.254" {
		t.Fatalf("last host = %s, want 10.0.3.254", last)
	}
	for _, h := range hosts {
		if h == "10.0.0.0" || h == "10.0.3.255" {
			t.Fatalf("Hosts() included network or broadcast address: %s", h)
		}
	}
}

func TestTargetHostsCombinesAndDedupes(t *testing.T) {
	source := fakeSource(
		InterfaceInfo{Name: "Ethernet1", Up: true, Addrs: []net.Addr{ipNet("10.0.1.5/22")}},
		InterfaceInfo{Name: "Ethernet2", Up: true, Addrs: []net.Addr{ipNet("192.168.1.10/24")}},
	)
	hosts, err := TargetHosts(source)
	if err != nil {
		t.Fatalf("TargetHosts: %v", err)
	}
	if len(hosts) != 1022+254 {
		t.Fatalf("len(hosts) = %d, want %d", len(hosts), 1022+254)
	}
	seen := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		if seen[h] {
			t.Fatalf("TargetHosts returned duplicate host %s", h)
		}
		seen[h] = true
	}
}
