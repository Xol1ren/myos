package netcfg

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
	"time"
	"unsafe"
)

var testMAC = net.HardwareAddr{0x52, 0x54, 0x00, 0x12, 0x34, 0x56}

// ioctl/ABI structs must match the kernel layout exactly (amd64).
func TestKernelStructSizes(t *testing.T) {
	if s := unsafe.Sizeof(ifreq{}); s != 40 {
		t.Errorf("ifreq %d, want 40", s)
	}
	if s := unsafe.Sizeof(rtentry{}); s != 120 {
		t.Errorf("rtentry %d, want 120", s)
	}
	var rt rtentry
	if o := unsafe.Offsetof(rt.flags); o != 56 {
		t.Errorf("rtentry.flags at %d, want 56", o)
	}
	if o := unsafe.Offsetof(rt.dev); o != 88 {
		t.Errorf("rtentry.dev at %d, want 88", o)
	}
}

func opts(pkt []byte) map[byte][]byte {
	m := map[byte][]byte{}
	o := pkt[240:]
	for i := 0; i < len(o) && o[i] != 255; {
		if o[i] == 0 {
			i++
			continue
		}
		m[o[i]] = o[i+2 : i+2+int(o[i+1])]
		i += 2 + int(o[i+1])
	}
	return m
}

func TestBuildDiscover(t *testing.T) {
	p := buildPacket(testMAC, 0xdeadbeef, dhcpDiscover, nil, nil)
	if p[0] != 1 || p[1] != 1 || p[2] != 6 {
		t.Fatalf("op/htype/hlen = %v", p[:3])
	}
	if binary.BigEndian.Uint32(p[4:8]) != 0xdeadbeef {
		t.Error("xid")
	}
	if binary.BigEndian.Uint16(p[10:12]) != 0x8000 {
		t.Error("broadcast flag must be set (no IP yet)")
	}
	if !bytes.Equal(p[28:34], testMAC) || !bytes.Equal(p[236:240], magicCookie) {
		t.Error("chaddr/cookie")
	}
	o := opts(p)
	if !bytes.Equal(o[53], []byte{dhcpDiscover}) {
		t.Errorf("msg type %v", o[53])
	}
	if _, ok := o[50]; ok {
		t.Error("discover must not request an address")
	}
	if !bytes.Equal(o[55], []byte{1, 3, 6, 51}) {
		t.Errorf("param request list %v", o[55])
	}
	if p[len(p)-1] != 255 {
		t.Error("missing END option")
	}
}

func TestBuildRequestCarriesOfferedIPAndServer(t *testing.T) {
	p := buildPacket(testMAC, 1, dhcpRequest, net.IPv4(10, 0, 2, 15), net.IPv4(10, 0, 2, 2))
	o := opts(p)
	if !bytes.Equal(o[53], []byte{dhcpRequest}) || !bytes.Equal(o[50], []byte{10, 0, 2, 15}) || !bytes.Equal(o[54], []byte{10, 0, 2, 2}) {
		t.Fatalf("opts %v", o)
	}
}

// serverReply fabricates a BOOTREPLY the way QEMU's slirp would.
func serverReply(xid uint32, msgType byte, mac net.HardwareAddr) []byte {
	p := make([]byte, 240)
	p[0] = 2
	binary.BigEndian.PutUint32(p[4:8], xid)
	copy(p[16:20], net.IPv4(10, 0, 2, 15).To4())
	copy(p[28:44], mac)
	copy(p[236:240], magicCookie)
	p = append(p, 53, 1, msgType)
	p = append(p, 1, 4, 255, 255, 255, 0)
	p = append(p, 3, 4, 10, 0, 2, 2)
	p = append(p, 6, 8, 10, 0, 2, 3, 8, 8, 8, 8)
	p = append(p, 51, 4, 0, 0, 0x0e, 0x10) // 3600s
	p = append(p, 54, 4, 10, 0, 2, 2)
	return append(p, 255)
}

func TestParseOfferAndAck(t *testing.T) {
	for _, mt := range []byte{dhcpOffer, dhcpAck} {
		r, err := parseReply(serverReply(42, mt, testMAC), 42, testMAC)
		if err != nil {
			t.Fatal(err)
		}
		l := r.lease
		if r.msgType != mt || !l.IP.Equal(net.IPv4(10, 0, 2, 15)) || !l.Router.Equal(net.IPv4(10, 0, 2, 2)) ||
			!l.Server.Equal(net.IPv4(10, 0, 2, 2)) || l.Time != time.Hour || len(l.DNS) != 2 || !l.DNS[1].Equal(net.IPv4(8, 8, 8, 8)) {
			t.Fatalf("%+v", r)
		}
		if l.PrefixLen() != 24 || l.Mask.String() != "ffffff00" {
			t.Errorf("mask %s prefix %d", l.Mask, l.PrefixLen())
		}
	}
}

func TestParseReplyRejects(t *testing.T) {
	good := serverReply(7, dhcpOffer, testMAC)
	if _, err := parseReply(good, 8, testMAC); err == nil {
		t.Error("wrong xid accepted")
	}
	if _, err := parseReply(good, 7, net.HardwareAddr{1, 2, 3, 4, 5, 6}); err == nil {
		t.Error("wrong chaddr accepted")
	}
	if _, err := parseReply(good[:100], 7, testMAC); err == nil {
		t.Error("short packet accepted")
	}
	req := append([]byte(nil), good...)
	req[0] = 1 // BOOTREQUEST, not a reply
	if _, err := parseReply(req, 7, testMAC); err == nil {
		t.Error("request accepted as reply")
	}
	bad := append([]byte(nil), good...)
	bad[236] = 0
	if _, err := parseReply(bad, 7, testMAC); err == nil {
		t.Error("bad magic cookie accepted")
	}
}

func TestParseReplyTruncatedOptionsDoNotPanic(t *testing.T) {
	p := serverReply(1, dhcpOffer, testMAC)
	for n := 241; n < len(p); n++ {
		_, _ = parseReply(p[:n], 1, testMAC) // must not panic on any cut
	}
	nak, err := parseReply(serverReply(1, dhcpNak, testMAC), 1, testMAC)
	if err != nil || nak.msgType != dhcpNak {
		t.Fatalf("%+v %v", nak, err)
	}
}

func TestPrefixLen(t *testing.T) {
	for _, c := range []struct {
		mask net.IPMask
		want int
	}{
		{net.CIDRMask(8, 32), 8}, {net.CIDRMask(24, 32), 24}, {net.CIDRMask(32, 32), 32}, {net.CIDRMask(0, 32), 0},
		{net.IPMask{255, 0, 255, 0}, -1}, // non-contiguous
		{net.CIDRMask(64, 128), -1},      // not IPv4
	} {
		if got := (&Lease{Mask: c.mask}).PrefixLen(); got != c.want {
			t.Errorf("%v -> %d, want %d", c.mask, got, c.want)
		}
	}
}

func TestLinkNameTooLong(t *testing.T) {
	if err := LinkUp("this-name-is-way-too-long"); err == nil {
		t.Fatal("expected error for oversized interface name")
	}
}

func TestInvalidMACLengthRejected(t *testing.T) {
	good := serverReply(7, dhcpOffer, testMAC)
	longMAC := make(net.HardwareAddr, 20)
	if _, err := parseReply(good, 7, longMAC); err == nil {
		t.Fatal("expected error for 20-byte non-Ethernet MAC in parseReply")
	}
	if _, err := DHCP(&net.Interface{Name: "ib0", HardwareAddr: longMAC}, 1, func(string, ...any) {}); err == nil {
		t.Fatal("expected error for 20-byte non-Ethernet MAC in DHCP")
	}
}
