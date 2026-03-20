//go:build windows
// +build windows

package main

import (
	"fmt"
	"net"
	"sync"
)

// The scoring listener only processes UDP packets that originated from a local
// IP address (i.e. ProScore running on the same machine). localIPs is refreshed
// every 90 seconds to handle DHCP renewals.

var (
	localIPsMu sync.RWMutex
	localIPs   []net.IP
)

func initLocalIPs() error {
	ifaces, err := net.Interfaces()
	if err != nil {
		return err
	}
	var ips []net.IP
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil {
				ips = append(ips, ip4)
			}
		}
	}
	localIPsMu.Lock()
	localIPs = ips
	localIPsMu.Unlock()
	if len(ips) == 0 {
		return fmt.Errorf("no non-loopback IPv4 addresses found")
	}
	return nil
}

func isLocalIP(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	localIPsMu.RLock()
	defer localIPsMu.RUnlock()
	for _, local := range localIPs {
		if local.Equal(ip4) {
			return true
		}
	}
	return false
}
