import React, { useEffect, useRef, useState, useCallback } from 'react';

const ROOT_URL = '/ui/api';

// RFB stream parser that buffers across WebSocket messages.
// The proxy sends raw RFB protocol bytes as WebSocket binary messages.
// A single FramebufferUpdate (5.2MB) arrives as many 16KB messages,
// so we must buffer and parse the RFB byte stream continuously.
export default function VNCViewer() {
  const canvasRef = useRef(null);
  const wsRef = useRef(null);
  const streamBufRef = useRef(null);      // accumulated bytes
  const parseStateRef = useRef(null);      // RFB parser state machine
  const [status, setStatus] = useState('disconnected');
  const [error, setError] = useState(null);
  const [stats, setStats] = useState({ frames: 0, bytes: 0 });

  // Initialize parser state
  const initParser = useCallback(() => {
    streamBufRef.current = new Uint8Array(0);
    parseStateRef.current = {
      width: 1404,
      height: 1872,
      bytesPerPixel: 2,
      // Parser state: 'name' (skip initial name msg), then 'message'
      phase: 'name',
      nameBytesRemaining: 0,
    };
  }, []);

  // Append new data to the stream buffer
  const appendData = useCallback((newData) => {
    const old = streamBufRef.current;
    const combined = new Uint8Array(old.length + newData.length);
    combined.set(old);
    combined.set(newData, old.length);
    streamBufRef.current = combined;
  }, []);

  // Consume N bytes from the front of the buffer
  const consume = useCallback((n) => {
    const buf = streamBufRef.current;
    streamBufRef.current = buf.slice(n);
  }, []);

  // Render a RAW RGB565 rectangle to the canvas
  const renderRect = useCallback((x, y, w, h, pixelData) => {
    const container = canvasRef.current;
    if (!container) return;
    const canvas = container.querySelector('canvas');
    if (!canvas) return;
    const ctx = canvas.getContext('2d');

    const numPixels = w * h;
    const imgData = ctx.createImageData(w, h);
    const view = new DataView(pixelData.buffer, pixelData.byteOffset, pixelData.byteLength);

    for (let i = 0; i < numPixels; i++) {
      const offset = i * 2;
      if (offset + 1 >= view.byteLength) break;
      // reMarkable uses little-endian RGB565 (despite what ServerInit says)
      const pixel = view.getUint16(offset, true); // little-endian
      const r = (pixel >> 11) & 0x1f;
      const g = (pixel >> 5) & 0x3f;
      const b = pixel & 0x1f;
      imgData.data[i * 4] = (r << 3) | (r >> 2);
      imgData.data[i * 4 + 1] = (g << 2) | (g >> 4);
      imgData.data[i * 4 + 2] = (b << 3) | (b >> 2);
      imgData.data[i * 4 + 3] = 255;
    }

    ctx.putImageData(imgData, x, y);
  }, []);

  // Parse the RFB byte stream from the buffer
  const parseStream = useCallback(() => {
    const state = parseStateRef.current;
    if (!state) return;

    let keepParsing = true;
    while (keepParsing) {
      const buf = streamBufRef.current;
      const view = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);

      if (state.phase === 'name') {
        // First message: 4-byte length + name string
        // e.g. 00 00 00 0e + "reMarkable rfb"
        if (buf.byteLength < 4) { keepParsing = false; break; }
        const nameLen = view.getUint32(0);
        if (nameLen > 0 && nameLen < 100 && 4 + nameLen <= buf.byteLength) {
          const name = new TextDecoder().decode(buf.slice(4, 4 + nameLen));
          console.log('[VNC] Server name:', name);
          consume(4 + nameLen);
          state.phase = 'message';
        } else if (nameLen === 0) {
          consume(4);
          state.phase = 'message';
        } else {
          // Not a name message — skip to message parsing
          state.phase = 'message';
        }
        continue;
      }

      if (state.phase === 'message') {
        // Need at least 1 byte for message type
        if (buf.byteLength < 1) { keepParsing = false; break; }
        const msgType = view.getUint8(0);

        if (msgType === 0) {
          // FramebufferUpdate: msgType(1) + padding(1) + numRects(2) + rect data
          if (buf.byteLength < 4) { keepParsing = false; break; }
          const numRects = view.getUint16(2);

          // Parse rectangles
          let offset = 4;
          let rectsParsed = 0;
          let incomplete = false;

          for (let r = 0; r < numRects; r++) {
            // Need 12 bytes for rect header
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
              // RAW: w * h * bytesPerPixel pixel data
              const pixelBytes = w * h * state.bytesPerPixel;
              if (offset + pixelBytes > buf.byteLength) {
                // Not enough data yet — wait for more
                incomplete = true;
                break;
              }
              const pixelData = buf.slice(offset, offset + pixelBytes);
              renderRect(x, y, w, h, pixelData);
              offset += pixelBytes;
              rectsParsed++;
            } else if (encoding === -223) {
              // DesktopSize pseudo-encoding — no pixel data
              state.width = w;
              state.height = h;
              const canvas = canvasRef.current?.querySelector('canvas');
              if (canvas) {
                canvas.width = w;
                canvas.height = h;
              }
              rectsParsed++;
            } else if (encoding === -239) {
              // Cursor pseudo-encoding
              const pixelBytes = w * h * state.bytesPerPixel;
              const maskBytes = Math.ceil((w * h) / 8);
              if (offset + pixelBytes + maskBytes > buf.byteLength) {
                incomplete = true;
                break;
              }
              offset += pixelBytes + maskBytes;
              rectsParsed++;
            } else {
              console.warn('[VNC] Unknown encoding:', encoding);
              // Can't determine size — abort this frame
              offset = buf.byteLength;
              rectsParsed = numRects;
              break;
            }
          }

          if (incomplete) {
            // Wait for more data — don't consume anything
            keepParsing = false;
            break;
          }

          // All rects parsed — consume the bytes
          consume(offset);
          setStats(prev => ({
            frames: prev.frames + 1,
            bytes: prev.bytes + offset,
          }));
        } else if (msgType === 1) {
          // SetColourMapEntries: msgType(1) + padding(2) + firstColour(2) + numColours(2) + colour data
          if (buf.byteLength < 8) { keepParsing = false; break; }
          const numColours = view.getUint16(6);
          const totalLen = 8 + numColours * 6;
          if (buf.byteLength < totalLen) { keepParsing = false; break; }
          consume(totalLen);
        } else if (msgType === 2) {
          // Bell: 1 byte
          consume(1);
        } else if (msgType === 3) {
          // ServerCutText: msgType(1) + padding(3) + length(4) + text
          if (buf.byteLength < 8) { keepParsing = false; break; }
          const length = view.getUint32(4);
          const totalLen = 8 + length;
          if (buf.byteLength < totalLen) { keepParsing = false; break; }
          consume(totalLen);
        } else {
          // Unknown message type — skip 1 byte and try to resync
          console.warn('[VNC] Unknown msg type:', msgType, 'at offset 0, buf length:', buf.byteLength);
          consume(1);
        }
      }
    }
  }, [consume, renderRect]);

  const connectVNC = useCallback(() => {
    if (wsRef.current) {
      wsRef.current.close();
      wsRef.current = null;
    }

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

    // Set up canvas
    const container = canvasRef.current;
    if (container) {
      container.innerHTML = '';
      const canvas = document.createElement('canvas');
      canvas.width = 1404;
      canvas.height = 1872;
      canvas.style.maxWidth = '100%';
      canvas.style.maxHeight = '100%';
      canvas.style.imageRendering = 'pixelated';
      canvas.style.background = '#fff';
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

    ws.onerror = (e) => {
      setError('WebSocket error');
      setStatus('error');
      console.error('[VNC] WS error:', e);
    };

    ws.onclose = (e) => {
      setStatus('disconnected');
      if (e.code !== 1000) {
        setError(`Connection closed (code: ${e.code})`);
      }
      wsRef.current = null;
    };
  }, [appendData, parseStream, initParser]);

  const disconnectVNC = useCallback(() => {
    if (wsRef.current) {
      wsRef.current.close();
      wsRef.current = null;
    }
    setStatus('disconnected');
  }, []);

  useEffect(() => {
    return () => {
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