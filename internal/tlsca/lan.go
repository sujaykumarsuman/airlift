package tlsca

import (
	"errors"
	"net"
	"sort"
)

// ErrNoLAN is returned when no usable IPv4 interface address is found.
var ErrNoLAN = errors.New("no LAN IPv4 address found; use --bind")

// LANIPv4s lists the IPv4 addresses of interfaces that are up and neither
// loopback nor link-local, private ranges first. The first entry is the
// default bind address; all of them go into the leaf's SANs.
func LANIPv4s() ([]net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []net.IP
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
				continue
			}
			out = append(out, ip)
		}
	}
	if len(out) == 0 {
		return nil, ErrNoLAN
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].IsPrivate() && !out[j].IsPrivate()
	})
	return out, nil
}
