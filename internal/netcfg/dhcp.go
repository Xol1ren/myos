package netcfg

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
)

// Lease is the result of a successful DHCP exchange.
type Lease struct {
	IP     net.IP
	Mask   net.IPMask
	Router net.IP
	DNS    []net.IP
	Server net.IP
	Time   time.Duration
}

// PrefixLen returns the CIDR prefix length of the lease's netmask (-1 if the
// mask is not a contiguous IPv4 mask).
func (l *Lease) PrefixLen() int {
	ones, bits := l.Mask.Size()
	if bits != 32 {
		return -1
	}
	return ones
}

const (
	dhcpDiscover = 1
	dhcpOffer    = 2
	dhcpRequest  = 3
	dhcpAck      = 5
	dhcpNak      = 6
)

var magicCookie = []byte{0x63, 0x82, 0x53, 0x63}

func buildPacket(mac net.HardwareAddr, xid uint32, msgType byte, reqIP, server net.IP) []byte {
	p := make([]byte, 240, 300)
	p[0], p[1], p[2] = 1, 1, 6 // BOOTREQUEST, ethernet, hlen
	binary.BigEndian.PutUint32(p[4:8], xid)
	binary.BigEndian.PutUint16(p[10:12], 0x8000) // ask for broadcast replies (we have no IP yet)
	copy(p[28:44], mac)
	copy(p[236:240], magicCookie)
	p = append(p, 53, 1, msgType)
	p = append(p, 61, 7, 1)
	p = append(p, mac...)
	if reqIP != nil {
		p = append(p, 50, 4)
		p = append(p, reqIP.To4()...)
	}
	if server != nil {
		p = append(p, 54, 4)
		p = append(p, server.To4()...)
	}
	p = append(p, 55, 4, 1, 3, 6, 51) // mask, router, dns, lease time
	p = append(p, 12, 5, 'm', 'y', 'o', 's', 0)
	return append(p, 255)
}

type reply struct {
	msgType byte
	lease   Lease
}

func parseReply(p []byte, xid uint32, mac net.HardwareAddr) (*reply, error) {
	if len(p) < 241 || p[0] != 2 || binary.BigEndian.Uint32(p[4:8]) != xid ||
		!bytes.Equal(p[28:28+len(mac)], mac) || !bytes.Equal(p[236:240], magicCookie) {
		return nil, errors.New("not our reply")
	}
	r := &reply{}
	r.lease.IP = net.IP(append([]byte(nil), p[16:20]...))
	opts := p[240:]
	for i := 0; i < len(opts); {
		code := opts[i]
		if code == 255 {
			break
		}
		if code == 0 {
			i++
			continue
		}
		if i+1 >= len(opts) || i+2+int(opts[i+1]) > len(opts) {
			break
		}
		v := opts[i+2 : i+2+int(opts[i+1])]
		switch code {
		case 53:
			if len(v) == 1 {
				r.msgType = v[0]
			}
		case 1:
			if len(v) == 4 {
				r.lease.Mask = net.IPMask(append([]byte(nil), v...))
			}
		case 3:
			if len(v) >= 4 {
				r.lease.Router = net.IP(append([]byte(nil), v[:4]...))
			}
		case 6:
			for j := 0; j+4 <= len(v); j += 4 {
				r.lease.DNS = append(r.lease.DNS, net.IP(append([]byte(nil), v[j:j+4]...)))
			}
		case 51:
			if len(v) == 4 {
				r.lease.Time = time.Duration(binary.BigEndian.Uint32(v)) * time.Second
			}
		case 54:
			if len(v) == 4 {
				r.lease.Server = net.IP(append([]byte(nil), v...))
			}
		}
		i += 2 + int(opts[i+1])
	}
	return r, nil
}

// DHCP performs DISCOVER/OFFER/REQUEST/ACK on ifc without configuring it.
// The UDP socket is bound to the device so it works before any IP exists.
func DHCP(ifc *net.Interface, attempts int, logf func(string, ...any)) (*Lease, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, ifc.Name)
			if serr == nil {
				serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
			}
			if serr == nil {
				serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
			}
		}); err != nil {
			return err
		}
		return serr
	}}
	conn, err := lc.ListenPacket(context.Background(), "udp4", "0.0.0.0:68")
	if err != nil {
		return nil, fmt.Errorf("dhcp listen: %w", err)
	}
	defer conn.Close()

	bcast := &net.UDPAddr{IP: net.IPv4bcast, Port: 67}
	var xidb [4]byte
	_, _ = rand.Read(xidb[:])
	xid := binary.BigEndian.Uint32(xidb[:])

	// exchange sends pkt and waits for a reply of type want (or NAK).
	exchange := func(pkt []byte, want byte, timeout time.Duration) (*reply, error) {
		if _, err := conn.WriteTo(pkt, bcast); err != nil {
			return nil, err
		}
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
		buf := make([]byte, 1500)
		for {
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				return nil, err
			}
			r, perr := parseReply(buf[:n], xid, ifc.HardwareAddr)
			if perr != nil {
				continue
			}
			if r.msgType == dhcpNak {
				return nil, errors.New("server sent NAK")
			}
			if r.msgType == want {
				return r, nil
			}
		}
	}

	var lastErr error
	for a := 1; a <= attempts; a++ {
		logf("dhcp: DISCOVER on %s (attempt %d/%d)", ifc.Name, a, attempts)
		timeout := time.Duration(a)*time.Second + time.Second
		offer, err := exchange(buildPacket(ifc.HardwareAddr, xid, dhcpDiscover, nil, nil), dhcpOffer, timeout)
		if err != nil {
			lastErr = err
			continue
		}
		ack, err := exchange(buildPacket(ifc.HardwareAddr, xid, dhcpRequest, offer.lease.IP, offer.lease.Server), dhcpAck, timeout)
		if err != nil {
			lastErr = err
			continue
		}
		l := ack.lease
		if l.Mask == nil {
			l.Mask = net.CIDRMask(24, 32)
		}
		if l.Server == nil {
			l.Server = offer.lease.Server
		}
		return &l, nil
	}
	return nil, fmt.Errorf("dhcp failed: %w", lastErr)
}
