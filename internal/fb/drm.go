package fb

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unsafe"

	"myos/internal/sys"
)

// DRMDevice represents an active DRM/KMS graphics device with a dumb buffer.
type DRMDevice struct {
	Canvas // offscreen back buffer, 0x00RRGGBB pixels

	file     *os.File
	fd       int
	mem      []byte   // mmap of dumb buffer
	hw32     []uint32 // 32-bit slice over mem
	stride   int      // pitch in bytes
	stride32 int      // pitch / 4

	cardPath string
	handle   uint32 // GEM dumb buffer handle
	fbID     uint32 // DRM framebuffer object ID
	crtcID   uint32 // DRM CRTC ID
	connID   uint32 // DRM connector ID
	mode     drmModeInfo

	origCRTC    drmCRTC
	hasOrigCRTC bool
}

// Width of the visible resolution.
func (d *DRMDevice) Width() int { return d.Canvas.W }

// Height of the visible resolution.
func (d *DRMDevice) Height() int { return d.Canvas.H }

// Surface returns the pointer to the underlying back-buffer canvas.
func (d *DRMDevice) Surface() *Canvas { return &d.Canvas }

// Backend returns the human-readable backend descriptor.
func (d *DRMDevice) Backend() string {
	modeStr := strings.TrimRight(string(d.mode.Name[:]), "\x00")
	if modeStr == "" {
		modeStr = fmt.Sprintf("%dx%d", d.Canvas.W, d.Canvas.H)
	}
	return fmt.Sprintf("DRM/KMS (%s @ %s)", filepath.Base(d.cardPath), modeStr)
}

// CardPath returns the device file path (e.g. /dev/dri/card0).
func (d *DRMDevice) CardPath() string { return d.cardPath }

// ModeName returns the active display mode name.
func (d *DRMDevice) ModeName() string {
	return strings.TrimRight(string(d.mode.Name[:]), "\x00")
}

// Blit copies the offscreen canvas to video memory in one pass.
func (d *DRMDevice) Blit() {
	if d.hw32 == nil {
		return
	}
	w, h := d.Canvas.W, d.Canvas.H
	src := d.Canvas.Pix
	for y := 0; y < h; y++ {
		copy(d.hw32[y*d.stride32:y*d.stride32+w], src[y*w:(y+1)*w])
	}
	// Notify driver of damage rectangle if supported (virtual GPUs like bochs/qxl)
	var dirty drmFBDirtyCmd
	dirty.fbID = d.fbID
	_ = sys.Ioctl(d.fd, drmIoctlModeDirtyFB, unsafe.Pointer(&dirty))
}

// Close restores the previous CRTC, unmaps memory, destroys the dumb buffer,
// and closes the file descriptor. It is safe to call twice.
func (d *DRMDevice) Close() error {
	var firstErr error
	if d.mem != nil {
		// Attempt to restore original CRTC if it had a valid mode and active FB
		if d.hasOrigCRTC && d.origCRTC.modeValid != 0 && d.origCRTC.fbID != 0 {
			connID := d.connID
			d.origCRTC.setConnectorsPtr = uint64(uintptr(unsafe.Pointer(&connID)))
			d.origCRTC.countConnectors = 1
			_ = sys.Ioctl(d.fd, drmIoctlModeSetCRTC, unsafe.Pointer(&d.origCRTC))
		}
		d.hw32 = nil
		if err := syscall.Munmap(d.mem); err != nil && firstErr == nil {
			firstErr = err
		}
		d.mem = nil
	}
	if d.fbID != 0 {
		fbID := d.fbID
		if err := sys.Ioctl(d.fd, drmIoctlModeRmFB, unsafe.Pointer(&fbID)); err != nil && firstErr == nil {
			firstErr = err
		}
		d.fbID = 0
	}
	if d.handle != 0 {
		var destroyReq drmDestroyDumb
		destroyReq.handle = d.handle
		if err := sys.Ioctl(d.fd, drmIoctlModeDestroyDumb, unsafe.Pointer(&destroyReq)); err != nil && firstErr == nil {
			firstErr = err
		}
		d.handle = 0
	}
	if d.file != nil {
		_ = sys.Ioctl(d.fd, drmIoctlDropMaster, nil)
		if err := d.file.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		d.file = nil
	}
	return firstErr
}

// FindDRMCards returns all primary card device paths in /dev/dri in sorted order.
func FindDRMCards() []string {
	entries, err := os.ReadDir("/dev/dri")
	if err != nil {
		return nil
	}
	var cards []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "card") {
			cards = append(cards, filepath.Join("/dev/dri", name))
		}
	}
	sort.Strings(cards)
	return cards
}

// OpenDRM opens a DRM card device, sets up dumb buffer KMS modesetting,
// and returns a ready-to-render DRMDevice. If path is empty, it searches /dev/dri.
func OpenDRM(path string) (*DRMDevice, error) {
	if path == "" {
		cards := FindDRMCards()
		if len(cards) == 0 {
			return nil, fmt.Errorf("no DRM cards found in /dev/dri")
		}
		var errs []string
		for _, c := range cards {
			dev, err := openDRMCard(c)
			if err == nil {
				return dev, nil
			}
			errs = append(errs, fmt.Sprintf("%s: %v", c, err))
		}
		return nil, fmt.Errorf("probe DRM cards failed: %s", strings.Join(errs, "; "))
	}
	return openDRMCard(path)
}

func openDRMCard(path string) (*DRMDevice, error) {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	fd := int(f.Fd())

	// Acquire DRM master if available (ignore error if already master or unprivileged)
	_ = sys.Ioctl(fd, drmIoctlSetMaster, nil)

	// Verify dumb buffer capability
	var capReq drmGetCap
	capReq.capability = drmCapDumbBuffer
	if err := sys.Ioctl(fd, drmIoctlGetCap, unsafe.Pointer(&capReq)); err != nil || capReq.value == 0 {
		f.Close()
		return nil, fmt.Errorf("device %s does not support dumb buffers", path)
	}

	// 1. Query resources count
	var res drmCardRes
	if err := sys.Ioctl(fd, drmIoctlModeGetResources, unsafe.Pointer(&res)); err != nil {
		f.Close()
		return nil, fmt.Errorf("DRM_IOCTL_MODE_GETRESOURCES: %w", err)
	}
	if res.countConnectors == 0 || res.countCRTCs == 0 {
		f.Close()
		return nil, fmt.Errorf("DRM device %s has %d connectors and %d CRTCs", path, res.countConnectors, res.countCRTCs)
	}

	// 2. Fetch full resources lists
	fbIDs := make([]uint32, res.countFBs)
	crtcIDs := make([]uint32, res.countCRTCs)
	connIDs := make([]uint32, res.countConnectors)
	encIDs := make([]uint32, res.countEncoders)

	if len(fbIDs) > 0 {
		res.fbIDPtr = uint64(uintptr(unsafe.Pointer(&fbIDs[0])))
	}
	res.crtcIDPtr = uint64(uintptr(unsafe.Pointer(&crtcIDs[0])))
	res.connectorIDPtr = uint64(uintptr(unsafe.Pointer(&connIDs[0])))
	if len(encIDs) > 0 {
		res.encoderIDPtr = uint64(uintptr(unsafe.Pointer(&encIDs[0])))
	}
	if err := sys.Ioctl(fd, drmIoctlModeGetResources, unsafe.Pointer(&res)); err != nil {
		f.Close()
		return nil, fmt.Errorf("DRM_IOCTL_MODE_GETRESOURCES data: %w", err)
	}

	// 3. Find an active/connected connector
	var selectedConn drmGetConnector
	var selectedModes []drmModeInfo
	var selectedConnID uint32
	foundConnector := false

	// Pass 1: DRM_MODE_CONNECTED; Pass 2 fallback: DRM_MODE_UNKNOWNCONNECTION
	for pass := 1; pass <= 2 && !foundConnector; pass++ {
		for _, cid := range connIDs {
			var conn drmGetConnector
			conn.connectorID = cid
			if err := sys.Ioctl(fd, drmIoctlModeGetConnector, unsafe.Pointer(&conn)); err != nil {
				continue
			}
			if pass == 1 && conn.connection != drmModeConnected {
				continue
			}
			if pass == 2 && conn.connection != drmModeUnknownConnection {
				continue
			}
			if conn.countModes == 0 {
				continue
			}

			// Populate modes and encoders for this connector
			modes := make([]drmModeInfo, conn.countModes)
			conn.modesPtr = uint64(uintptr(unsafe.Pointer(&modes[0])))
			var encs []uint32
			if conn.countEncoders > 0 {
				encs = make([]uint32, conn.countEncoders)
				conn.encodersPtr = uint64(uintptr(unsafe.Pointer(&encs[0])))
			}
			// Do not request properties during modesetting probe (avoids EFAULT when propsPtr is nil)
			conn.countProps = 0
			conn.propsPtr = 0
			conn.propValuesPtr = 0
			if err := sys.Ioctl(fd, drmIoctlModeGetConnector, unsafe.Pointer(&conn)); err != nil {
				continue
			}
			selectedConn = conn
			selectedModes = modes
			selectedConnID = cid
			foundConnector = true
			break
		}
	}
	if !foundConnector || len(selectedModes) == 0 {
		f.Close()
		return nil, fmt.Errorf("no connected connector found on %s", path)
	}

	// 4. Select mode
	chosenMode := selectBestMode(selectedModes)
	w := int(chosenMode.HDisplay)
	h := int(chosenMode.VDisplay)
	if w <= 0 || h <= 0 {
		f.Close()
		return nil, fmt.Errorf("invalid mode resolution %dx%d", w, h)
	}

	// 5. Select CRTC
	targetCRTCID := selectCRTC(fd, &selectedConn, crtcIDs)
	if targetCRTCID == 0 {
		f.Close()
		return nil, fmt.Errorf("no usable CRTC found for connector %d", selectedConnID)
	}

	// Save original CRTC config if possible
	var origCRTC drmCRTC
	origCRTC.crtcID = targetCRTCID
	hasOrigCRTC := false
	if err := sys.Ioctl(fd, drmIoctlModeGetCRTC, unsafe.Pointer(&origCRTC)); err == nil {
		hasOrigCRTC = true
	}

	// 6. Create dumb buffer (32-bit XRGB)
	var createReq drmCreateDumb
	createReq.width = uint32(w)
	createReq.height = uint32(h)
	createReq.bpp = 32
	if err := sys.Ioctl(fd, drmIoctlModeCreateDumb, unsafe.Pointer(&createReq)); err != nil {
		f.Close()
		return nil, fmt.Errorf("DRM_IOCTL_MODE_CREATE_DUMB: %w", err)
	}
	if createReq.pitch < uint32(w*4) || createReq.pitch%4 != 0 || createReq.size < uint64(createReq.pitch)*uint64(h) {
		var destroyReq drmDestroyDumb
		destroyReq.handle = createReq.handle
		_ = sys.Ioctl(fd, drmIoctlModeDestroyDumb, unsafe.Pointer(&destroyReq))
		f.Close()
		return nil, fmt.Errorf("invalid dumb buffer geometry: pitch=%d (min %d), size=%d", createReq.pitch, w*4, createReq.size)
	}

	// 7. Add Framebuffer (try ADDFB first, fallback to ADDFB2)
	var fbID uint32
	var fbCmd drmFBCmd
	fbCmd.width = uint32(w)
	fbCmd.height = uint32(h)
	fbCmd.pitch = createReq.pitch
	fbCmd.bpp = 32
	fbCmd.depth = 24
	fbCmd.handle = createReq.handle
	if err := sys.Ioctl(fd, drmIoctlModeAddFB, unsafe.Pointer(&fbCmd)); err == nil {
		fbID = fbCmd.fbID
	} else {
		var fbCmd2 drmFBCmd2
		fbCmd2.width = uint32(w)
		fbCmd2.height = uint32(h)
		fbCmd2.pixelFormat = drmFormatXRGB8888
		fbCmd2.handles[0] = createReq.handle
		fbCmd2.pitches[0] = createReq.pitch
		if err2 := sys.Ioctl(fd, drmIoctlModeAddFB2, unsafe.Pointer(&fbCmd2)); err2 != nil {
			var destroyReq drmDestroyDumb
			destroyReq.handle = createReq.handle
			_ = sys.Ioctl(fd, drmIoctlModeDestroyDumb, unsafe.Pointer(&destroyReq))
			f.Close()
			return nil, fmt.Errorf("register DRM FB (ADDFB: %v; ADDFB2: %w)", err, err2)
		}
		fbID = fbCmd2.fbID
	}

	// 8. Map dumb buffer offset
	var mapReq drmMapDumb
	mapReq.handle = createReq.handle
	if err := sys.Ioctl(fd, drmIoctlModeMapDumb, unsafe.Pointer(&mapReq)); err != nil {
		_ = sys.Ioctl(fd, drmIoctlModeRmFB, unsafe.Pointer(&fbID))
		var destroyReq drmDestroyDumb
		destroyReq.handle = createReq.handle
		_ = sys.Ioctl(fd, drmIoctlModeDestroyDumb, unsafe.Pointer(&destroyReq))
		f.Close()
		return nil, fmt.Errorf("DRM_IOCTL_MODE_MAP_DUMB: %w", err)
	}

	// 9. mmap dumb buffer into process address space
	mem, err := syscall.Mmap(fd, int64(mapReq.offset), int(createReq.size), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		_ = sys.Ioctl(fd, drmIoctlModeRmFB, unsafe.Pointer(&fbID))
		var destroyReq drmDestroyDumb
		destroyReq.handle = createReq.handle
		_ = sys.Ioctl(fd, drmIoctlModeDestroyDumb, unsafe.Pointer(&destroyReq))
		f.Close()
		return nil, fmt.Errorf("mmap dumb buffer: %w", err)
	}

	// 10. Set CRTC mode
	connIDVal := selectedConnID
	var setCRTC drmCRTC
	setCRTC.crtcID = targetCRTCID
	setCRTC.fbID = fbID
	setCRTC.x = 0
	setCRTC.y = 0
	setCRTC.setConnectorsPtr = uint64(uintptr(unsafe.Pointer(&connIDVal)))
	setCRTC.countConnectors = 1
	setCRTC.modeValid = 1
	setCRTC.mode = chosenMode

	if err := sys.Ioctl(fd, drmIoctlModeSetCRTC, unsafe.Pointer(&setCRTC)); err != nil {
		_ = syscall.Munmap(mem)
		_ = sys.Ioctl(fd, drmIoctlModeRmFB, unsafe.Pointer(&fbID))
		var destroyReq drmDestroyDumb
		destroyReq.handle = createReq.handle
		_ = sys.Ioctl(fd, drmIoctlModeDestroyDumb, unsafe.Pointer(&destroyReq))
		f.Close()
		return nil, fmt.Errorf("DRM_IOCTL_MODE_SETCRTC: %w", err)
	}

	stride32 := int(createReq.pitch) / 4
	hw32 := unsafe.Slice((*uint32)(unsafe.Pointer(&mem[0])), len(mem)/4)

	dev := &DRMDevice{
		Canvas:      NewCanvas(w, h),
		file:        f,
		fd:          fd,
		mem:         mem,
		hw32:        hw32,
		stride:      int(createReq.pitch),
		stride32:    stride32,
		cardPath:    path,
		handle:      createReq.handle,
		fbID:        fbID,
		crtcID:      targetCRTCID,
		connID:      selectedConnID,
		mode:        chosenMode,
		origCRTC:    origCRTC,
		hasOrigCRTC: hasOrigCRTC,
	}
	return dev, nil
}

// selectBestMode picks the preferred display mode if available, or the first valid mode.
func selectBestMode(modes []drmModeInfo) drmModeInfo {
	if len(modes) == 0 {
		return drmModeInfo{}
	}
	for _, m := range modes {
		if m.Type&drmModeTypePreferred != 0 {
			return m
		}
	}
	return modes[0]
}

// selectCRTC resolves a CRTC ID for the chosen connector.
func selectCRTC(fd int, conn *drmGetConnector, crtcIDs []uint32) uint32 {
	if len(crtcIDs) == 0 {
		return 0
	}
	// If connector is already bound to an encoder with an active CRTC
	if conn.encoderID != 0 {
		var enc drmGetEncoder
		enc.encoderID = conn.encoderID
		if err := sys.Ioctl(fd, drmIoctlModeGetEncoder, unsafe.Pointer(&enc)); err == nil {
			if enc.crtcID != 0 {
				return enc.crtcID
			}
			for i, cid := range crtcIDs {
				if enc.possibleCRTCs&(1<<i) != 0 {
					return cid
				}
			}
		}
	}
	// Check all encoders supported by the connector
	if conn.countEncoders > 0 {
		encs := make([]uint32, conn.countEncoders)
		conn.encodersPtr = uint64(uintptr(unsafe.Pointer(&encs[0])))
		conn.countModes = 0
		conn.modesPtr = 0
		conn.countProps = 0
		conn.propsPtr = 0
		conn.propValuesPtr = 0
		if err := sys.Ioctl(fd, drmIoctlModeGetConnector, unsafe.Pointer(conn)); err == nil {
			for _, eid := range encs {
				var enc drmGetEncoder
				enc.encoderID = eid
				if err := sys.Ioctl(fd, drmIoctlModeGetEncoder, unsafe.Pointer(&enc)); err == nil {
					for i, cid := range crtcIDs {
						if enc.possibleCRTCs&(1<<i) != 0 {
							return cid
						}
					}
				}
			}
		}
	}
	// Default to first CRTC
	return crtcIDs[0]
}

// OpenDevice selects the display backend according to preference:
// 1. If preferred points to a DRM card (e.g. contains "card" or starts with "/dev/dri/"), opens DRM.
// 2. If preferred points to a framebuffer (e.g. contains "fb"), opens Framebuffer.
// 3. In auto mode ("" or "auto"): probes DRM first, then falls back to Framebuffer.
// Returns a clear diagnostic error explaining what failed if neither backend is available.
func OpenDevice(preferred string) (Device, error) {
	if preferred != "" && preferred != "auto" {
		if strings.Contains(preferred, "card") || strings.HasPrefix(preferred, "/dev/dri") {
			return OpenDRM(preferred)
		}
		if strings.Contains(preferred, "fb") {
			return Open(preferred)
		}
	}

	// Auto mode: DRM preferred, Framebuffer fallback
	var drmErr, fbErr error

	drmDev, err := OpenDRM("")
	if err == nil {
		return drmDev, nil
	}
	drmErr = err

	fbDev, err := Open("/dev/fb0")
	if err == nil {
		return fbDev, nil
	}
	fbErr = err

	return nil, fmt.Errorf("no usable display backend (DRM: %v; Framebuffer: %v)", drmErr, fbErr)
}
