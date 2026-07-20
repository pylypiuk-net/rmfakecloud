import React, { useEffect, useRef, useState, useCallback } from 'react';
import pako from 'pako';

const ROOT_URL = '/ui/api';

// RFB stream viewer for reMarkable native screen share.
// The proxy captures the tablet's VNC broadcast, does the RFB handshake,
// and pipes the raw RFB byte stream to this viewer via the VNCHub.
//
// Key design decisions:
//   - First WebSocket message is an "RFBF" meta header with pixel format from ServerInit
//   - E-ink inversion: 0x0000 = white (no ink), 0xFFFF = black (full ink)
//   - Supports RAW and HEXTILE encodings (server picks one)
//   - Buffers across WebSocket messages (RFB is a byte stream, WS is message-based)
export default function VNCViewer() {
  const canvasRef = useRef(null);
  const wsRef = useRef(null);
  const streamBufRef = useRef(null);
  const parseStateRef = useRef(null);
  const manualDisconnectRef = useRef(false);
  const reconnectRef = useRef(null);
  const reconnectFnRef = useRef(null);
  const [status, setStatus] = useState('disconnected');
  const [error, setError] = useState(null);
  const [stats, setStats] = useState({ frames: 0, bytes: 0 });

  const initParser = useCallback(() => {
    streamBufRef.current = new Uint8Array(0);
    parseStateRef.current = {
      width: 1404,
      height: 1872,
      bpp: 32,       // will be set from RFBF meta
      bytesPerPixel: 4,
      bigEndian: 0,
      redMax: 255,
      greenMax: 255,
      blueMax: 255,
      redShift: 0,
      greenShift: 8,
      blueShift: 16,
      gotMeta: false,
      phase: 'meta', // start expecting RFBF meta
      zrleInflate: null, // persistent pako Inflate for ZRLE zlib stream
    };
  }, []);

  const appendData = useCallback((newData) => {
    const old = streamBufRef.current;
    const combined = new Uint8Array(old.length + newData.length);
    combined.set(old);
    combined.set(newData, old.length);
    streamBufRef.current = combined;
  }, []);

  const consume = useCallback((n) => {
    streamBufRef.current = streamBufRef.current.slice(n);
  }, []);

  // Convert a pixel value to RGBA.
  // Native VNC server sends standard RGB (0=black, max=white).
  // No e-ink inversion needed — the VNC server already provides correct colors.
  const pixelToRGBA = useCallback((pixel, state) => {
    const r = (pixel >> state.redShift) & state.redMax;
    const g = (pixel >> state.greenShift) & state.greenMax;
    const b = (pixel >> state.blueShift) & state.blueMax;

    // Expand to 8-bit
    const r8 = Math.round((r / state.redMax) * 255);
    const g8 = Math.round((g / state.greenMax) * 255);
    const b8 = Math.round((b / state.blueMax) * 255);

    return [r8, g8, b8, 255];
  }, []);

  // Render a RAW rectangle to canvas
  const renderRAW = useCallback((x, y, w, h, pixelData, state) => {
    const container = canvasRef.current;
    if (!container) return;
    const canvas = container.querySelector('canvas');
    if (!canvas) return;
    const ctx = canvas.getContext('2d');

    const numPixels = w * h;
    const imgData = ctx.createImageData(w, h);
    const view = new DataView(pixelData.buffer, pixelData.byteOffset, pixelData.byteLength);
    const bypp = state.bytesPerPixel;

    for (let i = 0; i < numPixels; i++) {
      const offset = i * bypp;
      if (offset + bypp > view.byteLength) break;

      let pixel;
      if (bypp === 4) {
        pixel = view.getUint32(offset, state.bigEndian === 0);
      } else if (bypp === 2) {
        pixel = view.getUint16(offset, state.bigEndian === 0);
      } else if (bypp === 1) {
        pixel = view.getUint8(offset);
      } else {
        pixel = view.getUint16(offset, state.bigEndian === 0);
      }

      const [r, g, b, a] = pixelToRGBA(pixel, state);
      imgData.data[i * 4] = r;
      imgData.data[i * 4 + 1] = g;
      imgData.data[i * 4 + 2] = b;
      imgData.data[i * 4 + 3] = a;
    }

    ctx.putImageData(imgData, x, y);
  }, [pixelToRGBA]);

  // Render a HEXTILE rectangle (encoding 5)
  // HEXTILE divides the rect into 16x16 tiles, each tile has a subencoding mask.
  const renderHEXTILE = useCallback((x, y, w, h, pixelData, state) => {
    const container = canvasRef.current;
    if (!container) return;
    const canvas = container.querySelector('canvas');
    if (!canvas) return;
    const ctx = canvas.getContext('2d');

    const view = new DataView(pixelData.buffer, pixelData.byteOffset, pixelData.byteLength);
    const bypp = state.bytesPerPixel;
    let offset = 0;

    // Read background and foreground colors (persist across tiles)
    let bgPixel = 0;
    let fgPixel = 0;

    for (let ty = 0; ty < h; ty += 16) {
      for (let tx = 0; tx < w; tx += 16) {
        if (offset + 1 > view.byteLength) return;

        const subencoding = view.getUint8(offset);
        offset++;

        const tw = Math.min(16, w - tx);
        const th = Math.min(16, h - ty);
        const tilePixels = tw * th;

        // Read background color if specified
        if (subencoding & 2) {
          if (offset + bypp > view.byteLength) return;
          if (bypp === 4) bgPixel = view.getUint32(offset, state.bigEndian === 0);
          else if (bypp === 2) bgPixel = view.getUint16(offset, state.bigEndian === 0);
          else bgPixel = view.getUint8(offset);
          offset += bypp;
        }

        // Read foreground color if specified
        if (subencoding & 4) {
          if (offset + bypp > view.byteLength) return;
          if (bypp === 4) fgPixel = view.getUint32(offset, state.bigEndian === 0);
          else if (bypp === 2) fgPixel = view.getUint16(offset, state.bigEndian === 0);
          else fgPixel = view.getUint8(offset);
          offset += bypp;
        }

        if (subencoding & 1) {
          // RAW tile: read tw*th pixels
          const tileBytes = tilePixels * bypp;
          if (offset + tileBytes > view.byteLength) return;
          const tileData = pixelData.slice(offset, offset + tileBytes);
          renderRAW(x + tx, y + ty, tw, th, tileData, state);
          offset += tileBytes;
        } else {
          // Subrect-encoded tile
          const numSubrects = (subencoding & 8) ? view.getUint8(offset) : 0;
          if (subencoding & 8) offset++;

          // Fill tile with background
          const [br, bg, bb, ba] = pixelToRGBA(bgPixel, state);
          const tileImg = ctx.createImageData(tw, th);
          for (let i = 0; i < tilePixels; i++) {
            tileImg.data[i * 4] = br;
            tileImg.data[i * 4 + 1] = bg;
            tileImg.data[i * 4 + 2] = bb;
            tileImg.data[i * 4 + 3] = ba;
          }

          // Draw subrects
          for (let s = 0; s < numSubrects; s++) {
            if (subencoding & 16) {
              // Colored subrect: read color
              if (offset + bypp > view.byteLength) return;
              if (bypp === 4) fgPixel = view.getUint32(offset, state.bigEndian === 0);
              else if (bypp === 2) fgPixel = view.getUint16(offset, state.bigEndian === 0);
              else fgPixel = view.getUint8(offset);
              offset += bypp;
            }
            if (offset + 2 > view.byteLength) return;
            const sx = (view.getUint8(offset) >> 4) & 0xf;
            const sy = view.getUint8(offset) & 0xf;
            offset++;
            const sw = ((view.getUint8(offset) >> 4) & 0xf) + 1;
            const sh = (view.getUint8(offset) & 0xf) + 1;
            offset++;

            const [sr, sg, sb, sa] = pixelToRGBA(fgPixel, state);
            for (let r = sy; r < sy + sh && r < th; r++) {
              for (let c = sx; c < sx + sw && c < tw; c++) {
                const idx = (r * tw + c) * 4;
                tileImg.data[idx] = sr;
                tileImg.data[idx + 1] = sg;
                tileImg.data[idx + 2] = sb;
                tileImg.data[idx + 3] = sa;
              }
            }
          }

          ctx.putImageData(tileImg, x + tx, y + ty);
        }
      }
    }
  }, [pixelToRGBA, renderRAW]);

  // Render a ZRLE rectangle (encoding 16).
  // ZRLE: 4-byte zlib length + zlib-compressed data.
  // Decompressed data = 64x64 tiles, each with a subencoding byte.
  // Subencoding:
  //   0 = raw pixels
  //   1 = single color (RLE of entire tile)
  //   2-127 = palette + RLE
  //   128-255 = palette + raw subrects (subencoding & 0x7f = num colors)
  const renderZRLE = useCallback((x, y, w, h, pixelData, state) => {
    const container = canvasRef.current;
    if (!container) return;
    const canvas = container.querySelector('canvas');
    if (!canvas) return;
    const ctx = canvas.getContext('2d');

    // First 4 bytes = zlib data length
    const view = new DataView(pixelData.buffer, pixelData.byteOffset, pixelData.byteLength);
    if (pixelData.byteLength < 4) return;
    const zlibLen = view.getUint32(0);
    console.log('[VNC] ZRLE rect:', x, y, w, h, 'zlibLen:', zlibLen, 'dataLen:', pixelData.byteLength, 'first bytes:', Array.from(pixelData.slice(4, 12)).join(','));
    if (4 + zlibLen > pixelData.byteLength) {
      console.warn('[VNC] ZRLE: incomplete zlib data, need', 4 + zlibLen, 'have', pixelData.byteLength);
      return;
    }

    // ZRLE: proxy decompresses the persistent zlib stream and sends each rect
    // as standalone zlib (with 789c header). Simple pako.inflate() per rect.
    let newData;
    try {
      newData = pako.inflate(pixelData.slice(4, 4 + zlibLen));
    } catch (e) {
      console.warn('[VNC] ZRLE: inflate failed:', e.message);
      return;
    }
    if (!newData || newData.length === 0) {
      return;
    }

    const dv = new DataView(newData.buffer, newData.byteOffset, newData.byteLength);
    const bypp = state.bytesPerPixel;
    let offset = 0;

    // Helper: read a pixel from decompressed data
    const readPixel = (off) => {
      if (bypp === 4) return dv.getUint32(off, state.bigEndian === 0);
      if (bypp === 2) return dv.getUint16(off, state.bigEndian === 0);
      return dv.getUint8(off);
    };

    // Process 64x64 tiles
    for (let ty = 0; ty < h; ty += 64) {
      for (let tx = 0; tx < w; tx += 64) {
        const tw = Math.min(64, w - tx);
        const th = Math.min(64, h - ty);
        const tilePixels = tw * th;

        if (offset + 1 > dv.byteLength) return;

        const subenc = dv.getUint8(offset);
        if (ty === 0 && tx === 0) console.log('[VNC] ZRLE first tile subenc:', subenc, 'offset:', offset);
        offset++;

        if (subenc === 0) {
          // Raw tile
          const tileBytes = tilePixels * bypp;
          if (offset + tileBytes > dv.byteLength) return;
          const tileData = new Uint8Array(newData.buffer, newData.byteOffset + offset, tileBytes);
          renderRAW(x + tx, y + ty, tw, th, tileData, state);
          offset += tileBytes;
        } else if (subenc === 1) {
          // Single color (RLE of entire tile)
          if (offset + bypp > dv.byteLength) return;
          const pixel = readPixel(offset);
          offset += bypp;
          const [r, g, b, a] = pixelToRGBA(pixel, state);
          const tileImg = ctx.createImageData(tw, th);
          for (let i = 0; i < tilePixels; i++) {
            tileImg.data[i * 4] = r;
            tileImg.data[i * 4 + 1] = g;
            tileImg.data[i * 4 + 2] = b;
            tileImg.data[i * 4 + 3] = a;
          }
          ctx.putImageData(tileImg, x + tx, y + ty);
        } else if (subenc <= 127) {
          // Palette + RLE
          const palette = [];
          for (let p = 0; p < subenc; p++) {
            if (offset + bypp > dv.byteLength) return;
            palette.push(readPixel(offset));
            offset += bypp;
          }
          const tileImg = ctx.createImageData(tw, th);
          let idx = 0;
          while (idx < tilePixels) {
            if (offset + 1 > dv.byteLength) return;
            const idxByte = dv.getUint8(offset);
            offset++;
            const palIdx = idxByte & 0x7f;
            let runLen = 1;
            if (idxByte & 0x80) {
              if (offset + 1 > dv.byteLength) return;
              runLen = dv.getUint8(offset) + 1;
              offset++;
            }
            const [r, g, b, a] = pixelToRGBA(palette[palIdx], state);
            for (let r2 = 0; r2 < runLen && idx < tilePixels; r2++, idx++) {
              tileImg.data[idx * 4] = r;
              tileImg.data[idx * 4 + 1] = g;
              tileImg.data[idx * 4 + 2] = b;
              tileImg.data[idx * 4 + 3] = a;
            }
          }
          ctx.putImageData(tileImg, x + tx, y + ty);
        } else {
          // 128-255: palette + raw subrects
          const numColors = subenc & 0x7f;
          const palette = [];
          for (let p = 0; p < numColors; p++) {
            if (offset + bypp > dv.byteLength) return;
            palette.push(readPixel(offset));
            offset += bypp;
          }
          const tileImg = ctx.createImageData(tw, th);
          for (let i = 0; i < tilePixels; i++) {
            if (offset + 1 > dv.byteLength) return;
            const palIdx = dv.getUint8(offset);
            offset++;
            const [r, g, b, a] = pixelToRGBA(palette[palIdx], state);
            tileImg.data[i * 4] = r;
            tileImg.data[i * 4 + 1] = g;
            tileImg.data[i * 4 + 2] = b;
            tileImg.data[i * 4 + 3] = a;
          }
          ctx.putImageData(tileImg, x + tx, y + ty);
        }
      }
    }
  }, [pixelToRGBA, renderRAW]);
  // Parse the RFB byte stream from the buffer
  const parseStream = useCallback(() => {
    const state = parseStateRef.current;
    if (!state) return;

    let keepParsing = true;
    while (keepParsing) {
      const buf = streamBufRef.current;

      if (!state.gotMeta) {
        // Expect RFBF meta header (24 bytes): "RFBF" + width(2) + height(2) +
        // bpp(1) + depth(1) + bigEndian(1) + trueColor(1) + R/G/B max(2 each) +
        // R/G/B shift(1 each) + reserved(3)
        if (buf.byteLength < 24) { keepParsing = false; break; }
        const view = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);

        // Check magic
        if (buf[0] !== 'R'.charCodeAt(0) || buf[1] !== 'F'.charCodeAt(0) ||
            buf[2] !== 'B'.charCodeAt(0) || buf[3] !== 'F'.charCodeAt(0)) {
          console.warn('[VNC] No RFBF meta, starting message phase directly');
          state.gotMeta = true;
          state.phase = 'message';
          continue;
        }

        state.width = view.getUint16(4);
        state.height = view.getUint16(6);
        state.bpp = view.getUint8(8);
        state.bytesPerPixel = state.bpp / 8;
        state.bigEndian = view.getUint8(10);
        state.redMax = view.getUint16(12);
        state.greenMax = view.getUint16(14);
        state.blueMax = view.getUint16(16);
        state.redShift = view.getUint8(18);
        state.greenShift = view.getUint8(19);
        state.blueShift = view.getUint8(20);

        console.log('[VNC] RFBF meta:', {
          width: state.width, height: state.height, bpp: state.bpp,
          bytesPerPixel: state.bytesPerPixel, bigEndian: state.bigEndian,
          redMax: state.redMax, greenMax: state.greenMax, blueMax: state.blueMax,
          redShift: state.redShift, greenShift: state.greenShift, blueShift: state.blueShift,
        });

        // Resize canvas
        const canvas = canvasRef.current?.querySelector('canvas');
        if (canvas) {
          canvas.width = state.width;
          canvas.height = state.height;
        }

        consume(24);
        state.gotMeta = true;
        state.phase = 'message';
        continue;
      }

      if (state.phase === 'message') {
        const view = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
        if (buf.byteLength < 1) { keepParsing = false; break; }
        const msgType = view.getUint8(0);

        if (msgType === 0) {
          // FramebufferUpdate: msgType(1) + padding(1) + numRects(2) + rect data
          if (buf.byteLength < 4) { keepParsing = false; break; }
          const numRects = view.getUint16(2);

          let offset = 4;
          let incomplete = false;

          for (let r = 0; r < numRects; r++) {
            if (offset + 12 > buf.byteLength) {
              incomplete = true;
              break;
            }
            const x = view.getUint16(offset);
            const y = view.getUint16(offset + 2);
            const w = view.getUint16(offset + 4);
            const h = view.getUint16(offset + 6);
            const encoding = view.getInt32(offset + 8);
            offset += 12;

            if (encoding === 0) {
              // RAW
              const pixelBytes = w * h * state.bytesPerPixel;
              if (offset + pixelBytes > buf.byteLength) {
                incomplete = true;
                break;
              }
              const pixelData = buf.slice(offset, offset + pixelBytes);
              renderRAW(x, y, w, h, pixelData, state);
              offset += pixelBytes;
            } else if (encoding === 5) {
              // HEXTILE — variable size, parse from buffer
              // We need to compute total size; let renderHEXTILE consume it
              // For simplicity, consume all remaining data for this rect
              // (the server sends the full rect data contiguously)
              const rectData = buf.slice(offset);
              renderHEXTILE(x, y, w, h, rectData, state);
              // HEXTILE size is hard to predict; advance past all remaining
              // In practice, one rect per FramebufferUpdate on rM
              offset = buf.byteLength;
            } else if (encoding === 16) {
              // ZRLE: 4-byte zlib length + zlib-compressed tile data
              // The zlib length tells us exactly how much data to consume
              if (offset + 4 > buf.byteLength) {
                incomplete = true;
                break;
              }
              const zlibLen = view.getUint32(offset);
              const totalRectLen = 4 + zlibLen;
              if (offset + totalRectLen > buf.byteLength) {
                incomplete = true;
                break;
              }
              const rectData = buf.slice(offset, offset + totalRectLen);
              renderZRLE(x, y, w, h, rectData, state);
              offset += totalRectLen;
            } else if (encoding === -223) {
              // DesktopSize
              state.width = w;
              state.height = h;
              const canvas = canvasRef.current?.querySelector('canvas');
              if (canvas) { canvas.width = w; canvas.height = h; }
            } else if (encoding === -239) {
              // Cursor pseudo-encoding
              const pixelBytes = w * h * state.bytesPerPixel;
              const maskBytes = Math.ceil((w * h) / 8);
              if (offset + pixelBytes + maskBytes > buf.byteLength) {
                incomplete = true;
                break;
              }
              offset += pixelBytes + maskBytes;
            } else {
              console.warn('[VNC] Unknown encoding:', encoding);
              offset = buf.byteLength;
              break;
            }
          }

          if (incomplete) {
            keepParsing = false;
            break;
          }

          consume(offset);
          setStats(prev => ({
            frames: prev.frames + 1,
            bytes: prev.bytes + offset,
          }));
        } else if (msgType === 1) {
          // SetColourMapEntries
          if (buf.byteLength < 8) { keepParsing = false; break; }
          const numColours = view.getUint16(6);
          const totalLen = 8 + numColours * 6;
          if (buf.byteLength < totalLen) { keepParsing = false; break; }
          consume(totalLen);
        } else if (msgType === 2) {
          // Bell
          consume(1);
        } else if (msgType === 3) {
          // ServerCutText
          if (buf.byteLength < 8) { keepParsing = false; break; }
          const length = view.getUint32(4);
          const totalLen = 8 + length;
          if (buf.byteLength < totalLen) { keepParsing = false; break; }
          consume(totalLen);
        } else {
          console.warn('[VNC] Unknown msg type:', msgType);
          consume(1);
        }
      }
    }
  }, [consume, renderRAW, renderHEXTILE, renderZRLE]);

  const connectVNC = useCallback(() => {
    if (wsRef.current) {
      wsRef.current.close();
      wsRef.current = null;
    }
    manualDisconnectRef.current = false;

    setStatus('connecting');
    setError(null);
    setStats({ frames: 0, bytes: 0 });
    initParser();

    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const wsUrl = `${protocol}//${window.location.host}${ROOT_URL}/screenshare/vnc/stream`;
    const token = localStorage.getItem('authToken');
    const urlWithToken = token ? `${wsUrl}?token=${encodeURIComponent(token)}` : wsUrl;

    const ws = new WebSocket(urlWithToken);
    ws.binaryType = 'arraybuffer';
    wsRef.current = ws;

    const container = canvasRef.current;
    if (container) {
      container.innerHTML = '';
      const canvas = document.createElement('canvas');
      canvas.width = 1404;
      canvas.height = 1872;
      // Preserve aspect ratio — use height as the constraint since the
      // container is typically wider than tall. The canvas fills the
      // available height, and width is derived from the aspect ratio.
      canvas.style.height = '100%';
      canvas.style.width = 'auto';
      canvas.style.maxWidth = '100%';
      canvas.style.maxHeight = '100%';
      canvas.style.aspectRatio = '1404 / 1872';
      canvas.style.imageRendering = 'auto';
      canvas.style.background = '#fff';
      canvas.style.margin = '0 auto';
      canvas.style.display = 'block';
      canvas.style.flexShrink = '1';
      container.appendChild(canvas);
    }

    ws.onopen = () => {
      setStatus('connected');
      console.log('[VNC] WebSocket connected');
    };

    ws.onmessage = (e) => {
      if (e.data instanceof ArrayBuffer) {
        appendData(new Uint8Array(e.data));
        parseStream();
      }
    };

    ws.onerror = () => {
      // Don't set error here — onclose will handle reconnection
      console.warn('[VNC] WebSocket error');
    };

    ws.onclose = (e) => {
      setStatus('disconnected');
      if (e.code !== 1000) {
        setError(`Connection lost. Reconnecting...`);
      }
      wsRef.current = null;

      // Auto-reconnect after 2s if we didn't manually disconnect
      if (!manualDisconnectRef.current) {
        reconnectRef.current = setTimeout(() => {
          if (!manualDisconnectRef.current && reconnectFnRef.current) {
            console.log('[VNC] Auto-reconnecting...');
            reconnectFnRef.current();
          }
        }, 2000);
      }
    };
  }, [appendData, parseStream, initParser]);

  // Store the connect function in a ref for reconnect
  useEffect(() => {
    reconnectFnRef.current = connectVNC;
  }, [connectVNC]);

  const disconnectVNC = useCallback(() => {
    manualDisconnectRef.current = true;
    if (reconnectRef.current) {
      clearTimeout(reconnectRef.current);
      reconnectRef.current = null;
    }
    if (wsRef.current) {
      wsRef.current.close();
      wsRef.current = null;
    }
    setStatus('disconnected');
  }, []);

  useEffect(() => {
    return () => {
      manualDisconnectRef.current = true;
      if (reconnectRef.current) {
        clearTimeout(reconnectRef.current);
      }
      if (wsRef.current) {
        wsRef.current.close();
      }
    };
  }, []);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%' }}>
      <div style={{
        padding: '8px 16px',
        display: 'flex',
        alignItems: 'center',
        gap: '12px',
        borderBottom: '1px solid #e0e0e0',
        background: '#fafafa'
      }}>
        <button
          onClick={status === 'connected' ? disconnectVNC : connectVNC}
          style={{
            padding: '6px 16px',
            borderRadius: '4px',
            border: 'none',
            background: status === 'connected' ? '#e53e3e' : '#38a169',
            color: 'white',
            cursor: 'pointer',
            fontSize: '14px',
          }}
        >
          {status === 'connected' ? 'Disconnect' : 'Connect'}
        </button>
        <span style={{ fontSize: '14px', color: '#666' }}>
          Status: <strong style={{
            color: status === 'connected' ? '#38a169' : status === 'error' ? '#e53e3e' : '#666'
          }}>{status}</strong>
        </span>
        {status === 'connected' && (
          <span style={{ fontSize: '12px', color: '#999' }}>
            {stats.frames} frames · {(stats.bytes / 1024 / 1024).toFixed(1)} MB
          </span>
        )}
        {error && (
          <span style={{ fontSize: '13px', color: '#e53e3e' }}>{error}</span>
        )}
        <span style={{ fontSize: '12px', color: '#999', marginLeft: 'auto' }}>
          reMarkable Live View (VNC)
        </span>
      </div>
      <div
        ref={canvasRef}
        style={{
          flex: 1,
          display: 'flex',
          justifyContent: 'center',
          alignItems: 'center',
          background: '#f0f0f0',
          overflow: 'hidden',
        }}
      />
    </div>
  );
}