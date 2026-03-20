//go:build windows
// +build windows

package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
)

// searchMDNS browses for _http._tcp services for 5 seconds and returns an
// http://host:port URL. If exactly one service is found it is selected
// automatically; if multiple are found the user is prompted to choose.
func searchMDNS() string {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		appendLog("mDNS error: " + err.Error())
		return ""
	}

	entries := make(chan *zeroconf.ServiceEntry)
	var found []*zeroconf.ServiceEntry
	var wg sync.WaitGroup

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for e := range entries {
			found = append(found, e)
			if len(e.AddrIPv4) > 0 {
				appendLog(fmt.Sprintf("  mDNS: %s — %s:%d", e.Instance, e.AddrIPv4[0], e.Port))
			}
		}
	}()

	if err = resolver.Browse(ctx, "_http._tcp.", "local.", entries); err != nil {
		appendLog("mDNS browse error: " + err.Error())
		return ""
	}
	<-ctx.Done()
	wg.Wait()

	if len(found) == 0 {
		return ""
	}
	if len(found) == 1 && len(found[0].AddrIPv4) > 0 {
		ep := fmt.Sprintf("http://%s:%d", found[0].AddrIPv4[0], found[0].Port)
		appendLog("mDNS auto-selected: " + ep)
		return ep
	}

	var lines []string
	for i, s := range found {
		if len(s.AddrIPv4) > 0 {
			lines = append(lines, fmt.Sprintf("%d. %s  (%s:%d)", i+1, s.Instance, s.AddrIPv4[0], s.Port))
		}
	}
	choice := guiInputBoxTimeout("Select mDNS Service",
		"Multiple services found. Enter number:\n\n"+strings.Join(lines, "\n"), "1", 10*time.Second)
	var sel int
	fmt.Sscanf(choice, "%d", &sel)
	if sel < 1 || sel > len(found) || len(found[sel-1].AddrIPv4) == 0 {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", found[sel-1].AddrIPv4[0], found[sel-1].Port)
}
