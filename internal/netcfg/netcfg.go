// Package netcfg configures network interfaces with plain ioctls and a
// minimal DHCP client; no netlink library or external tools required.
package netcfg

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
	"unsafe"

	"myos/internal/sys"
)

const (
	siocGIFFLAGS = 0x8913
	siocSIFFLAGS = 0x8914
	siocSIFADDR  = 0x8916
	siocSIFNMASK = 0x891c
	siocADDRT    = 0x890b
	siocDELRT    = 0x890c
	siocSIFMTU   = 0x8922

	iffUp = 0x1

	rtfUp      = 0x1
	rtfGateway = 0x2
)

type ifreq struct {
	name [16]byte
	data [24]byte
}

type rtentry struct {
	pad1    uint64
	dst     syscall.RawSockaddrInet4
	gateway syscall.RawSockaddrInet4
	genmask syscall.RawSockaddrInet4
	flags   uint16
	pad2    int16
	pad3    uint64
	tos     uint8
	class   uint8
	pad4    [3]int16
	metric  int16
	dev     *byte
	mtu     uint64
	window  uint64
	irtt    uint16
}

func newReq(name string) (ifreq, error) {
	var r ifreq
	if len(name) >= len(r.name) {
		return r, fmt.Errorf("interface name too long: %q", name)
	}
	copy(r.name[:], name)
	return r, nil
}

func withSocket(f func(fd int) error) error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	return f(fd)
}

func sockaddrData(r *ifreq, ip net.IP) {
	binary.NativeEndian.PutUint16(r.data[0:2], syscall.AF_INET)
	copy(r.data[4:8], ip.To4())
}

// LinkUp sets IFF_UP on the named interface.
func LinkUp(name string) error {
	return withSocket(func(fd int) error {
		r, err := newReq(name)
		if err != nil {
			return err
		}
		if err := sys.Ioctl(fd, siocGIFFLAGS, unsafe.Pointer(&r)); err != nil {
			return fmt.Errorf("SIOCGIFFLAGS %s: %w", name, err)
		}
		fl := binary.NativeEndian.Uint16(r.data[0:2]) | iffUp
		binary.NativeEndian.PutUint16(r.data[0:2], fl)
		if err := sys.Ioctl(fd, siocSIFFLAGS, unsafe.Pointer(&r)); err != nil {
			return fmt.Errorf("SIOCSIFFLAGS %s: %w", name, err)
		}
		return nil
	})
}

// SetAddr assigns an IPv4 address and netmask.
func SetAddr(name string, ip net.IP, mask net.IPMask) error {
	return withSocket(func(fd int) error {
		r, err := newReq(name)
		if err != nil {
			return err
		}
		sockaddrData(&r, ip)
		if err := sys.Ioctl(fd, siocSIFADDR, unsafe.Pointer(&r)); err != nil {
			return fmt.Errorf("SIOCSIFADDR: %w", err)
		}
		r2, _ := newReq(name)
		sockaddrData(&r2, net.IP(mask))
		if err := sys.Ioctl(fd, siocSIFNMASK, unsafe.Pointer(&r2)); err != nil {
			return fmt.Errorf("SIOCSIFNETMASK: %w", err)
		}
		return nil
	})
}

// SetMTU sets the interface MTU.
func SetMTU(name string, mtu int) error {
	return withSocket(func(fd int) error {
		r, _ := newReq(name)
		binary.NativeEndian.PutUint32(r.data[0:4], uint32(mtu))
		return sys.Ioctl(fd, siocSIFMTU, unsafe.Pointer(&r))
	})
}

// AddDefaultRoute installs 0.0.0.0/0 via gw, replacing any prior default route.
func AddDefaultRoute(name string, gw net.IP) error {
	return withSocket(func(fd int) error {
		dev := append([]byte(name), 0)
		// Remove any existing default route first so gateway changes take effect cleanly
		delRT := rtentry{flags: rtfUp}
		delRT.dst.Family, delRT.genmask.Family = syscall.AF_INET, syscall.AF_INET
		_ = sys.Ioctl(fd, siocDELRT, unsafe.Pointer(&delRT))

		rt := rtentry{flags: rtfUp | rtfGateway, dev: &dev[0]}
		rt.dst.Family, rt.gateway.Family, rt.genmask.Family = syscall.AF_INET, syscall.AF_INET, syscall.AF_INET
		copy(rt.gateway.Addr[:], gw.To4())
		if err := sys.Ioctl(fd, siocADDRT, unsafe.Pointer(&rt)); err != nil && err != syscall.EEXIST {
			return fmt.Errorf("SIOCADDRT: %w", err)
		}
		return nil
	})
}

// Loopback brings lo up with 127.0.0.1/8.
func Loopback() error {
	if err := LinkUp("lo"); err != nil {
		return err
	}
	return SetAddr("lo", net.IPv4(127, 0, 0, 1), net.CIDRMask(8, 32))
}

// PrimaryInterface waits up to timeout for a non-loopback, Ethernet-like
// interface (drivers may be loaded asynchronously) and returns it.
func PrimaryInterface(timeout time.Duration) (*net.Interface, error) {
	deadline := time.Now().Add(timeout)
	for {
		ifs, _ := net.Interfaces()
		var fallback *net.Interface
		for i := range ifs {
			ifc := ifs[i]
			if ifc.Flags&net.FlagLoopback != 0 || len(ifc.HardwareAddr) != 6 {
				continue
			}
			if len(ifc.Name) >= 3 && (ifc.Name[:3] == "eth" || ifc.Name[:2] == "en") {
				return &ifc, nil
			}
			if fallback == nil {
				fallback = &ifc
			}
		}
		if fallback != nil {
			return fallback, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("no network interface found")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// WriteResolvConf writes /etc/resolv.conf for the given servers.
func WriteResolvConf(servers []net.IP) error {
	var b []byte
	for _, s := range servers {
		b = append(b, "nameserver "+s.String()+"\n"...)
	}
	if len(b) == 0 {
		return nil
	}
	return os.WriteFile("/etc/resolv.conf", b, 0o644)
}

// Configure brings the interface up, runs DHCP, and applies the lease.
func Configure(ifc *net.Interface, logf func(string, ...any)) (*Lease, error) {
	if err := LinkUp(ifc.Name); err != nil {
		return nil, err
	}
	// Give the PHY a moment to report carrier before sending.
	time.Sleep(500 * time.Millisecond)
	lease, err := DHCP(ifc, 4, logf)
	if err != nil {
		return nil, err
	}
	if err := SetAddr(ifc.Name, lease.IP, lease.Mask); err != nil {
		return nil, err
	}
	if lease.Router != nil {
		if err := AddDefaultRoute(ifc.Name, lease.Router); err != nil {
			return lease, err
		}
	}
	if err := WriteResolvConf(lease.DNS); err != nil {
		return lease, err
	}
	return lease, nil
}
