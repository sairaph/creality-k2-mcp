package discovery

import (
	"encoding/binary"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
)

// InterfaceInfo is the minimal interface/address shape this package needs,
// decoupled from net.Interface (and its own Addrs call) so tests can supply
// fixed data without any real network device.
type InterfaceInfo struct {
	Name  string
	Up    bool
	Addrs []net.Addr
}

// InterfaceSource returns the current machine's network interfaces. The
// default, SystemInterfaces, wraps net.Interfaces; tests inject a fake
// instead of touching real hardware.
type InterfaceSource func() ([]InterfaceInfo, error)

// SystemInterfaces is the default InterfaceSource, reading this machine's
// real network interfaces.
func SystemInterfaces() ([]InterfaceInfo, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]InterfaceInfo, 0, len(ifaces))
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			// An interface this process cannot query right now (a
			// permissions issue, or a device disappearing mid-enumeration)
			// is skipped rather than failing the whole scan.
			continue
		}
		out = append(out, InterfaceInfo{
			Name:  ifc.Name,
			Up:    ifc.Flags&net.FlagUp != 0,
			Addrs: addrs,
		})
	}
	return out, nil
}

// virtualAdapterPattern matches interface names for adapters that are not a
// real LAN link: Hyper-V/vEthernet switches, WSL, Docker bridges,
// VirtualBox and VMware host-only/NAT adapters, Tailscale/ZeroTier/
// WireGuard, and generic tun/tap/utun/wg tunnel names. It is a case
// insensitive match, since real interface names vary by OS and driver (for
// example Windows reports "vEthernet (WSL)", Linux reports "docker0" or
// "br-xxxxxxxxxxxx", macOS reports "utun3").
var virtualAdapterPattern = regexp.MustCompile(
	`(?i)(vethernet|hyper-v|\bwsl\b|docker|^br-|vboxnet|virtualbox|vmware|vmnet|tailscale|zerotier|wireguard|^tun[0-9]*$|^tap[0-9]*$|^utun[0-9]*$|^wg[0-9]*$)`,
)

// isVirtualAdapter reports whether name looks like a virtual adapter that
// discovery should skip (plan-v0.1.0.md decision 3).
func isVirtualAdapter(name string) bool {
	return virtualAdapterPattern.MatchString(name)
}

// minPrefix is the smallest (most host-rich) prefix length discovery will
// ever scan: /22, 1022 usable hosts (plan-v0.1.0.md decision 3).
const minPrefix = 22

// Subnet is one deduplicated scan target produced by Subnets.
type Subnet struct {
	// Network is the subnet's network address (host bits zeroed).
	Network net.IP
	// Bits is the prefix length. It is always at least minPrefix: a real
	// subnet larger than /22 is narrowed to the /22 containing the
	// interface's address; a real subnet already /22 or smaller is used as
	// is, unchanged.
	Bits int
}

// String returns the subnet in CIDR notation.
func (s Subnet) String() string {
	return fmt.Sprintf("%s/%d", s.Network, s.Bits)
}

func (s Subnet) key() string {
	return s.Network.String() + "/" + strconv.Itoa(s.Bits)
}

// Hosts enumerates every usable host address in the subnet, ascending,
// excluding the network and broadcast addresses. A /22 yields 1022
// addresses (decision 3).
func (s Subnet) Hosts() []string {
	total := uint64(1) << uint(32-s.Bits)
	if total <= 2 {
		return nil
	}
	base := ipToUint32(s.Network)
	out := make([]string, 0, total-2)
	for i := uint64(1); i < total-1; i++ {
		out = append(out, uint32ToIP(base+uint32(i)).String())
	}
	return out
}

func ipToUint32(ip net.IP) uint32 {
	ip4 := ip.To4()
	return binary.BigEndian.Uint32(ip4)
}

func uint32ToIP(v uint32) net.IP {
	b := make(net.IP, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

// Subnets enumerates every up, non-loopback, non-link-local IPv4 interface
// subnet from source, skipping interfaces that look virtual by name
// (isVirtualAdapter) and capping every subnet at /22 (plan-v0.1.0.md
// decision 3): a subnet larger than /22 is narrowed to the /22 containing
// the interface's own address, so a home /16 or /8 network does not turn
// into a scan of millions of hosts; a subnet already /22 or smaller is used
// as is. The result is deduplicated by network/prefix across interfaces
// (two interfaces landing on the same range contribute one target) and
// sorted for deterministic output. A nil source defaults to
// SystemInterfaces.
func Subnets(source InterfaceSource) ([]Subnet, error) {
	if source == nil {
		source = SystemInterfaces
	}
	ifaces, err := source()
	if err != nil {
		return nil, err
	}

	seen := make(map[string]Subnet)
	for _, ifc := range ifaces {
		if !ifc.Up {
			continue
		}
		if isVirtualAdapter(ifc.Name) {
			continue
		}
		for _, addr := range ifc.Addrs {
			sub, ok := subnetForAddr(addr)
			if !ok {
				continue
			}
			seen[sub.key()] = sub
		}
	}

	out := make([]Subnet, 0, len(seen))
	for _, s := range seen {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

// subnetForAddr computes the capped subnet for one interface address, or
// ok=false when the address is not an IPv4 address discovery should ever
// target (loopback, link-local, IPv6, or an address with an unusable mask).
func subnetForAddr(addr net.Addr) (sub Subnet, ok bool) {
	ipnet, ok := addr.(*net.IPNet)
	if !ok {
		return Subnet{}, false
	}
	ip4 := ipnet.IP.To4()
	if ip4 == nil {
		return Subnet{}, false // IPv6, out of scope
	}
	if ip4.IsLoopback() {
		return Subnet{}, false
	}
	if ip4[0] == 169 && ip4[1] == 254 {
		return Subnet{}, false // link-local, 169.254.0.0/16
	}
	ones, bits := ipnet.Mask.Size()
	if bits != 32 || ones == 0 {
		// A zero-value or unusable mask should not happen for a real
		// interface address; treated as "no usable subnet" rather than
		// guessed at.
		return Subnet{}, false
	}
	capped := ones
	if capped < minPrefix {
		capped = minPrefix
	}
	network := ip4.Mask(net.CIDRMask(capped, 32))
	return Subnet{Network: network, Bits: capped}, true
}

// TargetHosts computes every host address a scan should try: Subnets from
// source, each expanded to its usable hosts, deduplicated across subnets.
func TargetHosts(source InterfaceSource) ([]string, error) {
	subnets, err := Subnets(source)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var hosts []string
	for _, s := range subnets {
		for _, h := range s.Hosts() {
			if seen[h] {
				continue
			}
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	return hosts, nil
}
