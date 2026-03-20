package main

import (
	"net"
	"sort"
)

func getIPAddresses() map[string][]string {
	result := make(map[string][]string)
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		want := net.FlagUp | net.FlagBroadcast
		if iface.Flags&want != want {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil {
				result[iface.Name] = append(result[iface.Name], ip4.String())
			}
		}
	}
	return result
}

func firstIP() string {
	addrs := getIPAddresses()
	keys := make([]string, 0, len(addrs))
	for k := range addrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return "localhost"
	}
	ips := addrs[keys[0]]
	if len(ips) == 0 {
		return "localhost"
	}
	return ips[0]
}
