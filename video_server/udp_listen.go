package main

import (
	"fmt"
	"log"
	"net"
	"time"

	"videoreview/shared"
)

// listenUDP binds all three UDP ports and dispatches each packet to the shared
// parsers. Only called when the -listen flag is passed.
// The scoring listener on the same machine will already be handling these ports
// if running; this is for standalone operation where the video server itself
// receives UDP directly from the network.
func listenUDP() {
	ports := []int{keypadUDPPort, ipadUDPPort, scoregen1Port}
	for _, port := range ports {
		p := port
		addr, _ := net.ResolveUDPAddr("udp4", fmt.Sprintf(":%d", p))
		conn, err := net.ListenUDP("udp4", addr)
		if err != nil {
			log.Fatalf("UDP listen error on port %d: %v", p, err)
		}
		log.Printf("Listening on UDP port %d", p)
		go func(c *net.UDPConn, port int) {
			buf := make([]byte, 65535)
			for {
				n, raddr, err := c.ReadFromUDP(buf)
				if err != nil {
					log.Printf("UDP read error (port %d): %v", port, err)
					continue
				}
				data := string(buf[:n])
				server := raddr.IP.String()

				// Port 23467 only carries SCOREGEN-LAST messages; discard others.
				if port == scoregen1Port && (len(data) < 13 || data[:13] != "SCOREGEN-LAST") {
					continue
				}

				var msg shared.ProScoreMessage
				var parseErr error
				switch port {
				case ipadUDPPort:
					msg, parseErr = shared.ParseXMLMessage(server, data, nil)
				default:
					msg, parseErr = shared.ParseCSVMessage(server, data)
				}
				if parseErr != nil {
					log.Printf("UDP parse error (port %d): %v", port, parseErr)
					continue
				}

				log.Printf("%s @ %s - %s: %s", server, time.Now().Format("15:04:05"), msg.Apparatus, msg.Status)
				saveEvent(msg)
				hub.broadcast(msg)
			}
		}(conn, p)
	}
}
