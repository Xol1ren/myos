# MYOS Release Candidate 3 (RC3) — Author Self-Review

**Review Target:** MYOS v0.3.0-rc3  
**Commit / Tag:** `f7cb318` / `v0.3.0-rc3`  
**Author:** Adilet Baktybekov  
**Date:** November 7, 2026  
**Host Environment:** Linux x86_64, QEMU, Go 1.25 (`CGO_ENABLED=0`)

---

## 1. Summary & Status

### Status: **RC3 self-review: no known blockers at the time of writing**

A review and testing of the subsystem upgrades in MYOS RC3:
1. **Interactive Graphical PTY Terminal:** Pure-Go Linux pseudo-terminal allocation (`/dev/ptmx`, `/dev/pts/*`), session management (`setsid`, `TIOCSCTTY`), clean descriptor lifecycle, and ANSI/VT100 terminal grid emulation.
2. **Streaming HTTP/HTTPS Client (`fetch`):** Direct streaming to file/stdout with bounded O(1) buffer allocation (~16 KB), strict socket timeout deadlines, and recursion limits on cyclic HTTP redirects.
3. **Block Storage Scanner & Early Mounts (`disks`):** Multi-instance `devpts` mounting, non-blocking `/sys/class/block` inspections, and `BLKGETSIZE64` ioctl safety across storage and virtual devices.
4. **Desktop Concurrency:** Zero data races verified via `go test -race ./...`. Goroutine lifecycle leak benchmarks across GUI open-close cycles and QEMU hardware-accelerated compositing were not measured in this session.

No release blockers were identified. Architectural observations and planned optimizations are cataloged below for RC4.

---

## 2. Technical Findings by Subsystem

### 2.1 PTY & Subprocess Subsystem ([`internal/fb/terminal.go`](../internal/fb/terminal.go))

#### Resource & Descriptor Lifecycle
- **Master Allocation (`OpenPTY`):** Master descriptor is acquired via `/dev/ptmx` with automatic fallback to `/dev/pts/ptmx` using `O_RDWR | syscall.O_CLOEXEC`.
- **Unlock ioctl:** `TIOCSPTLCK` (`0x40045431`) is dispatched with `unlock = 0`.
- **Slave Allocation:** Utilizes modern Linux 4.13+ `TIOCGPTPEER` (`0x00005441`) with flags `O_RDWR | syscall.O_CLOEXEC`. On legacy environments, safely falls back to `TIOCGPTN` (`0x80045430`) followed by `/dev/pts/%d` node opening.
- **Window Size Synchronization:** Sets initial terminal dimensions via `TIOCSWINSZ` (`0x00005414`) mapping 70 columns × 20 rows (560×320 text area).
- **Descriptor Teardown:** `Terminal.Close()` implements double-close protection (`if t.closed { return nil }`), kills the child process if alive, and closes both `t.master` and `t.slave`. Descriptor teardown is implemented via explicit handle cleanup; automated multi-cycle FD polling benchmarks were not measured in this session.

#### Process Ownership & Zombie Reaping
- **POSIX Session Management:** Child shell is executed with:
  ```go
  cmd.SysProcAttr = &syscall.SysProcAttr{
      Setsid:  true,
      Setctty: true,
      Ctty:    int(t.slave.Fd()),
  }
  ```
  This creates a new session leader via `setsid(2)` and assigns the slave pseudo-terminal as the controlling terminal (`TIOCSCTTY`).
- **Zombie Reaping:** Integration with [`internal/proc/proc.go`](../internal/proc/proc.go):
  - PID 1 runs `proc.StartReaper()` which handles `SIGCHLD` and reaps orphaned or exited children via non-blocking `wait4(-1, &ws, WNOHANG, nil)`.
  - The reader goroutine in `terminal.go` also calls `cmd.Wait()` asynchronously. If PID 1's reaper claims the exit status first, Go's `cmd.Wait()` safely returns `ECHILD` without panic and sets `t.active = false`. Children do not linger as uncollected zombies.

#### ANSI/VT100 Decoder & Grid Memory Bounds
- **Grid Geometry:** Terminal buffer is backed by a 2D matrix `[][]Cell` initialized to fixed dimensions (`t.rows × t.cols`).
- **Bounds Clamping:** All cell mutations strictly verify `0 <= curX < cols` and `0 <= curY < rows`. Horizontal overflow triggers line-wrapping; vertical overflow invokes `scrollUp()` (shifting rows via slice copy and zeroing the bottom line).
- **Coordinate Sanitization:** The CSI position handler `\x1b[r;cH` uses `min(max(r-1, 0), t.rows-1)` and `min(max(c-1, 0), t.cols-1)`. Malformed, negative, or large coordinates are clamped within bounds.
- **Concurrency Safety:** The terminal grid, cursor state, and I/O streams are synchronized under `sync.Mutex` (`t.mu`). Reader goroutine writes and GUI canvas blits are synchronized.

---

### 2.2 Networking & HTTP Engine ([`internal/netcfg/http.go`](../internal/netcfg/http.go))

#### Streaming Memory Footprint
- **Direct Disk Streaming:** When `-o output_path` is specified, data is written directly to the filesystem handle via 16 KB chunks (`buf := make([]byte, 16*1024)` in raw mode, `io.CopyN` in chunked mode).
- **Streaming Buffer Footprint:** Data is streamed in chunks of 16 KB to avoid full-body buffering into memory (`io.ReadAll` is avoided). Specific heap allocation deltas across large payload downloads were not measured in this session.

#### Socket & Deadline Management
- **Connection Deadlines:** Initial TCP/TLS connection enforced via `dialer.Timeout` (default 15s).
- **Read/Write Deadlines:** Sockets are refreshed with `conn.SetDeadline(time.Now().Add(timeout))` prior to header reads and each block/chunk read.
- **Cyclic Redirect Protection:** Redirects (HTTP 301, 302, 303, 307, 308) are capped at a maximum of 5 hops (`if redirects > 5 { return nil, errors.New("fetch: too many redirects (>5)") }`), preventing infinite loops and stack exhaustion.

#### Transfer Speed Calculation
- In [`internal/shell/builtins.go`](../internal/shell/builtins.go), transfer speed is computed as:
  ```go
  rate := float64(res.BytesRead) / (time.Since(start).Seconds() + 0.001)
  ```
  The `+ 0.001` denominator offset eliminates division-by-zero risks on sub-millisecond local transfers.

---

### 2.3 Storage Scanner & Early Mounts ([`internal/sys/disk.go`](../internal/sys/disk.go), [`internal/sys/sys.go`](../internal/sys/sys.go))

#### Early Mounts Hygiene
- In `internal/sys/sys.go`, `MountEarly()` mounts:
  1. `devtmpfs` on `/dev`
  2. `devpts` on `/dev/pts` (`ptmxmode=0666,mode=620`)
  3. `proc` on `/proc`
  4. `sysfs` on `/sys`
  5. `tmpfs` on `/tmp` (`mode=1777`)
- **Mount Target Pre-creation:** `os.MkdirAll(m.Target, 0o755)` is executed dynamically inside the loop before each mount. This guarantees that `/dev/pts` is created *after* `devtmpfs` is mounted over `/dev`, resolving the previous mount masking bug.

#### Storage Probing Safety
- **Sysfs & Dev Node Scanning:** `ScanDisks()` reads `/sys/class/block/` and globs `/dev/sd*`, `/dev/vd*`, `/dev/nvme*`.
- **Ioctl Graceful Degradation:** When probing partitions or querying sizes via `BLKGETSIZE64` (`0x80081272`), failures on read-only, optical, or empty virtual media are caught cleanly (`err == nil` check); devices report `SizeBytes = 0` without failing the overall command.

---

## 3. Statement-Level Test Coverage

Coverage measured via `go test -cover -count=1 ./...`:

| Package | RC3 Coverage | Verification Notes |
| :--- | :--- | :--- |
| `myos/cmd/init` | **1.5%** | Integration entrypoint (PID 1 boot logic) |
| `myos/internal/fb` | **66.5%** | Terminal, PTY, and framebuffer canvas |
| `myos/internal/input` | **98.8%** | PS/2 & ImPS/2 mouse protocol suite |
| `myos/internal/monitor` | **82.7%** | Sysfs CPU, memory, interface monitor |
| `myos/internal/netcfg` | **51.1%** | Unit tests for HTTP/HTTPS engine |
| `myos/internal/proc` | **90.2%** | PID 1 child reaper & process spawning |
| `myos/internal/shell` | **29.0%** | Builtins (`disks`, `fetch`, `gui`) |
| `myos/internal/sys` | **63.9%** | Block storage scanning & early mounts |

---

## 4. Performance & Resource Profile

All runtime benchmarks and resource metrics require live hardware or QEMU guest instrumentation; parameters not executed in automated testing are noted as not measured:

| Parameter | Status | Assessment |
| :--- | :--- | :--- |
| **Canvas Blit Time (1280×800)** | Not measured in this session | Implementation performs direct slice blit to framebuffer |
| **Max Theoretical Blit Rate** | Not measured in this session | Frame rate benchmark not executed |
| **Blit Heap Allocations** | Not measured in this session | In-place copy without per-frame slice allocations |
| **PTY Terminal Write Latency** | Not measured in this session | Direct write/read to master/slave PTY |
| **Multi-Cycle Goroutine Delta** | Not measured in this session | Explicit worker goroutine shutdown on close |
| **File Descriptor Lifecycle** | Not measured in this session | Explicit master and slave descriptor closure |
| **Init Binary Size (`build/rootfs/init`)** | **~4.9 MB** (stripped pure-Go binary) | Static binary built via `go build -trimpath -ldflags="-s -w"` |
| **Initramfs Archive (`initramfs.cpio.gz`)** | Not measured in this session | Generated via `cpio` + `gzip` |
| **Bootable Hybrid ISO (`myos.iso`)** | Not measured in this session | Requires host xorriso and distro kernel image |

---

## 5. Prioritized Technical Debt for RC4

1. **CSI Parameter Buffer Length Cap (Input Validation — Priority: Medium):**
   - *Current State:* `t.csiBuf` appends parameter bytes between `0-9` and `?` until a final byte arrives.
   - *Observation:* A rogue PTY stream emitting an unbounded stream of digits without terminating with an action letter could cause excessive heap allocation in `csiBuf`.
   - *RC4 Recommendation:* Cap `len(t.csiBuf)` at 64 bytes; if exceeded, reset `inCSI = false`.
2. **Terminal Scrollback Buffer (Feature Enhancement — Priority: Low):**
   - *Current State:* Lines scrolled past `t.rows` are discarded; the grid only stores active viewport lines.
   - *RC4 Recommendation:* Add an optional in-memory circular scrollback buffer (e.g. 500 lines) navigable via Shift-PageUp/PageDown or mouse wheel.
3. **Partition Scanning Expansion (Hardware Support — Priority: Low):**
   - *Current State:* `ScanDisks()` focuses on `sd*`, `vd*`, and `nvme*`.
   - *RC4 Recommendation:* Add support for legacy IDE (`/dev/hd*`), MMC/SD cards (`/dev/mmcblk*`), and optical drives (`/dev/sr*`).
