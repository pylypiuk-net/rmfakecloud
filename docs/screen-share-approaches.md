# Screen Share Approaches — History & Status

This document records all approaches attempted to make reMarkable 2 (firmware 3.3.2)
screen sharing work with the rmfakecloud web UI.

## Approach 1: Native VNC Proxy (RFB) — IN PROGRESS (revisit)

**Tag:** `vnc-proxy-rfb` (commit 40a53b3)
**Status:** RFB handshake works; pixel data investigation reopened.

### How it works
1. Tablet broadcasts UDP to `192.168.1.255:5901` when screen share is enabled
2. Proxy on tablet captures broadcast, extracts timestamp + SHA256(userIdHash)
3. Proxy connects to tablet's RFB server on port 5900 via SSL
4. RFB handshake: version exchange (`reM 001.001\n`), RM_AUTH (type 100),
   challenge = SHA256(timestamp + SHA256(auth0-userid))
5. Server sends ServerInit (width, height, pixel format, name "reMarkable rfb")
6. Proxy sends SetEncodings + FramebufferUpdateRequest
7. Server streams FramebufferUpdate rectangles (raw pixels)
8. Proxy pipes RFB stream to rmfakecloud VNCHub via WebSocket
9. Web UI viewer parses RFB frames, renders to canvas

### What worked
- UDP broadcast capture (after fixing IPv4/IPv6 socket issue)
- RFB version exchange (echoing `reM 001.001\n`)
- RM_AUTH challenge computation (SHA256(timestamp + SHA256(userId)))
- Auth result 0 (success)
- ServerInit received (1404×1872, name "reMarkable rfb")
- 21.8MB of pixel data flowing through to web UI

### What failed (and why it was wrong)
- **"All-zeros pixel data" conclusion was premature.** The nonZero counter showed 0
  non-zero bytes across 21MB. But:
  - `0x0000` = **white** on e-ink (not black). A mostly-white page has mostly-zero pixels.
  - The viewer did NOT apply e-ink color inversion, so valid data rendered as all-black.
  - The proxy sent `SetPixelFormat(RGB565, 16bpp)` which rmview does NOT do — rmview uses
    the server's default format. This may have caused the server to send a different format.
  - The proxy hardcoded 1404×1872 in FramebufferUpdateRequest, but ServerInit returned 0×0
    (likely a parsing issue — rmview reads width/height from ServerInit successfully).

### Reference
- rmview (https://github.com/bordaigorl/rmview, 826★) reverse-engineered this protocol
- rmview does NOT send SetPixelFormat — uses server default
- rmview requests HEXTILE, CORRE, ZRLE, RRE, RAW encodings
- rmview applies e-ink inversion: `0x0000` = white, `0xFFFF` = black

## Approach 2: Framebuffer Proxy (restream + ptrace + LZ4) — WORKING (superseded)

**Tag:** `framebuffer-proxy-working` (commit f0f11c1)
**Status:** Working end-to-end, but a workaround for Approach 1's misdiagnosis.

### How it works
1. `restream v1.5.0` binary on tablet attaches to xochitl via ptrace
2. restream reads xochitl's framebuffer from the anonymous mapping after /dev/fb0
3. restream LZ4-compresses the framebuffer and writes to a FIFO
4. `fbproxy` (Go binary) reads LZ4 from FIFO, decompresses with pierrec/lz4/v4
5. fbproxy sends complete raw frames (2,628,288 bytes each) via WebSocket to rmfakecloud
6. VNCHub fans out to web UI viewers
7. Viewer renders 4-bit grayscale (×17 scale), 90° CCW rotation

### What worked
- Full end-to-end: live tablet screen visible in web UI
- Verified via vision analysis: readable handwritten text, correct orientation
- Each WebSocket message is a complete raw frame (no reassembly needed)

### Drawbacks
- Streams constantly even with no viewer (no viewer-gated streaming)
- Requires restream v1.5.0 binary (not in Toltec, manual download)
- ptrace attachment is fragile (breaks if xochitl restarts)
- LZ4 decompression in proxy adds complexity
- Bypasses native screen share entirely (custom capture path)
- Tablet must run fbproxy as background process (not a systemd service)

## Approach 3: Direct /dev/fb0 read — FAILED

**Status:** Abandoned.

### Why it failed
- `/dev/fb0` is e-ink waveform memory, not a standard framebuffer
- Reads produce only 7-8 unique grayscale values forming vertical bands
- The actual screen content is in xochitl's process memory (anonymous mapping)

## Approach 4: /proc/<pid>/mem read — FAILED

**Status:** Abandoned.

### Why it failed
- The /dev/fb0 mapping in xochitl's /proc/<pid>/maps has `rw-s` (shared device) flags
- Reading from /proc/<pid>/mem at this virtual address returns I/O error
- The kernel doesn't allow reading shared device mappings through /proc/<pid>/mem
- Only ptrace can read the process's view of shared mappings

## Key Learnings

1. **E-ink color inversion is critical**: `0x0000` = white (no ink), `0xFFFF` = black (full ink)
2. **ServerInit width/height may be 0×0** but actual dimensions come in FramebufferUpdate headers
3. **RM_AUTH requires 4-byte server prompt read** before sending challenge
4. **auth0-userid must be in JWT** — rmfakecloud's UserClaims must include it
5. **UDP socket must be IPv4** — `net.ListenUDP("udp4", ...)` not just `ResolveUDPAddr("udp4", ...)`
6. **restream outputs LZ4 frames to files/FIFOs but raw data to pipes** (O_RDWR makes it detect a pipe)
7. **FIFO must be opened O_WRONLY** in a goroutine, not O_RDWR
8. **Binary copy to tablet is fragile** — scp via nsenter truncates >32KB, must use cat|ssh pipe