package fb

// DRM ioctl request codes derived from Linux UAPI <drm/drm.h> and <drm/drm_mode.h>.
const (
	drmIoctlSetMaster        = 0x0000641E
	drmIoctlDropMaster       = 0x0000641F
	drmIoctlGetCap           = 0xC010640C
	drmIoctlModeGetResources = 0xC04064A0
	drmIoctlModeGetCRTC      = 0xC06864A1
	drmIoctlModeSetCRTC      = 0xC06864A2
	drmIoctlModeGetConnector = 0xC05064A7
	drmIoctlModeGetEncoder   = 0xC01464A6
	drmIoctlModeAddFB        = 0xC01C64AE
	drmIoctlModeAddFB2       = 0xC06864B8
	drmIoctlModeRmFB         = 0xC00464AF
	drmIoctlModePageFlip     = 0xC01864B0
	drmIoctlModeDirtyFB      = 0xC01864B1
	drmIoctlModeCreateDumb   = 0xC02064B2
	drmIoctlModeMapDumb      = 0xC01064B3
	drmIoctlModeDestroyDumb  = 0xC00464B4
)

// Capabilities, connector status, and mode flags from Linux UAPI.
const (
	drmCapDumbBuffer         = 1
	drmModeConnected         = 1
	drmModeDisconnected      = 2
	drmModeUnknownConnection = 3
	drmModeTypePreferred     = 8  // 1 << 3
	drmModeTypeDefault       = 16 // 1 << 4
	drmFormatXRGB8888        = 0x34325258
	drmFormatARGB8888        = 0x34325241
)

// drmCardRes mirrors struct drm_mode_card_res (64 bytes).
type drmCardRes struct {
	fbIDPtr         uint64
	crtcIDPtr       uint64
	connectorIDPtr  uint64
	encoderIDPtr    uint64
	countFBs        uint32
	countCRTCs      uint32
	countConnectors uint32
	countEncoders   uint32
	minWidth        uint32
	maxWidth        uint32
	minHeight       uint32
	maxHeight       uint32
}

// drmModeInfo mirrors struct drm_mode_modeinfo (68 bytes).
type drmModeInfo struct {
	Clock      uint32
	HDisplay   uint16
	HSyncStart uint16
	HSyncEnd   uint16
	HTotal     uint16
	HSkew      uint16
	VDisplay   uint16
	VSyncStart uint16
	VSyncEnd   uint16
	VTotal     uint16
	VScan      uint16
	VRefresh   uint32
	Flags      uint32
	Type       uint32
	Name       [32]byte
}

// drmCRTC mirrors struct drm_mode_crtc (104 bytes).
type drmCRTC struct {
	setConnectorsPtr uint64
	countConnectors  uint32
	crtcID           uint32
	fbID             uint32
	x                uint32
	y                uint32
	gammaSize        uint32
	modeValid        uint32
	mode             drmModeInfo
}

// drmGetConnector mirrors struct drm_mode_get_connector (80 bytes).
type drmGetConnector struct {
	encodersPtr     uint64
	modesPtr        uint64
	propsPtr        uint64
	propValuesPtr   uint64
	countModes      uint32
	countProps      uint32
	countEncoders   uint32
	encoderID       uint32
	connectorID     uint32
	connectorType   uint32
	connectorTypeID uint32
	connection      uint32
	mmWidth         uint32
	mmHeight        uint32
	subpixel        uint32
	pad             uint32
}

// drmGetEncoder mirrors struct drm_mode_get_encoder (20 bytes).
type drmGetEncoder struct {
	encoderID      uint32
	encoderType    uint32
	crtcID         uint32
	possibleCRTCs  uint32
	possibleClones uint32
}

// drmCreateDumb mirrors struct drm_mode_create_dumb (32 bytes).
type drmCreateDumb struct {
	height uint32
	width  uint32
	bpp    uint32
	flags  uint32
	handle uint32
	pitch  uint32
	size   uint64
}

// drmMapDumb mirrors struct drm_mode_map_dumb (16 bytes).
type drmMapDumb struct {
	handle uint32
	pad    uint32
	offset uint64
}

// drmDestroyDumb mirrors struct drm_mode_destroy_dumb (4 bytes).
type drmDestroyDumb struct {
	handle uint32
}

// drmFBCmd mirrors struct drm_mode_fb_cmd (28 bytes).
type drmFBCmd struct {
	fbID   uint32
	width  uint32
	height uint32
	pitch  uint32
	bpp    uint32
	depth  uint32
	handle uint32
}

// drmFBCmd2 mirrors struct drm_mode_fb_cmd2 (104 bytes).
type drmFBCmd2 struct {
	fbID        uint32
	width       uint32
	height      uint32
	pixelFormat uint32
	flags       uint32
	handles     [4]uint32
	pitches     [4]uint32
	offsets     [4]uint32
	modifier    [4]uint64
}

// drmFBDirtyCmd mirrors struct drm_mode_fb_dirty_cmd (24 bytes).
type drmFBDirtyCmd struct {
	fbID     uint32
	flags    uint32
	color    uint32
	numClips uint32
	clipsPtr uint64
}

// drmGetCap mirrors struct drm_get_cap (16 bytes).
type drmGetCap struct {
	capability uint64
	value      uint64
}
