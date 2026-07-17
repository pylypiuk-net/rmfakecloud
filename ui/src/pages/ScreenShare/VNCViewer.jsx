import React, { useEffect, useRef, useState, useCallback } from 'react';

const ROOT_URL = '/ui/api';

// Minimal RFB framebuffer decoder.
// The proxy on the tablet already completed the RFB handshake.
// We receive raw RFB protocol messages over WebSocket and render them.
// The reMarkable uses 16bpp RGB565 RAW encoding.
export default function VNCViewer() {
  const canvasRef = useRef(null);
  const wsRef = useRef(null);
  const rfbStateRef = useRef({
    width: 1404,
    height: 1872,
    bpp: 16,
    depth: 16,
    bigEndian: 0,
    trueColor: 1,
    redMax: 31,
    greenMax: 63,
    blueMax: 31,
    redShift: 11,
    greenShift: 5,
    blueShift: 0,
    bytesPerPixel: 2,
    pendingRects: 0,
    rectBytesRemaining: 0,
    currentRect: null,
    frameBuffer: null,
  });
  const [status, setStatus] = useState('disconnected');
  const [error, setError] = useState(null);

  const renderRect = useCallback((x, y, w, h, pixelData) => {
    const canvas = canvasRef.current?.querySelector('canvas');
    if (!canvas) return;
    const ctx = canvas.getContext('2d');
    const state = rfbStateRef.current;

    // Convert RGB565 pixel data to ImageData
    const imgData = ctx.createImageData(w, h);
    const pixels = new DataView(pixelData.buffer || pixelData);
    const numPixels = w * h;

    for (let i = 0; i < numPixels; i++) {
      const offset = i * 2;
      const pixel = pixels.getUint16(offset, false); // big-endian
      const r = (pixel >> 11) & 0x1f;
      const g = (pixel >> 5) & 0x3f;
      const b = pixel & 0x1f;
      // Scale to 8-bit
      imgData.data[i * 4] = (r << 3) | (r >> 2);     // 5->8
      imgData.data[i * 4 + 1] = (g << 2) | (g >> 4);  // 6->8
      imgData.data[i * 4 + 2] = (b << 3) | (b >> 2);  // 5->8
      imgData.data[i * 4 + 3] = 255;
    }

    ctx.putImageData(imgData, x, y);
  }, []);

  const handleMessage = useCallback((data) => {
    const view = new DataView(data);
    const state = rfbStateRef.current;

    if (data.byteLength < 1) return;

    const msgType = view.getUint8(0);

    switch (msgType) {
      case 0: { // FramebufferUpdate
        if (data.byteLength < 4) return;
        const numRects = view.getUint16(2);
        console.log(`FBUpdate: ${numRects} rects, ${data.byteLength} bytes`);
        
        let offset = 4;
        for (let r = 0; r < numRects && offset + 12 <= data.byteLength; r++) {
          const x = view.getUint16(offset);
          const y = view.getUint16(offset + 2);
          const w = view.getUint16(offset + 4);
          const h = view.getUint16(offset + 6);
          const encoding = view.getInt32(offset + 8); // signed
          offset += 12;

          console.log(`  Rect ${r}: ${w}x${h} at (${x},${y}), encoding=${encoding}`);

          if (encoding === 0) { // RAW
            const pixelBytes = w * h * state.bytesPerPixel;
            if (offset + pixelBytes <= data.byteLength) {
              const pixelData = new Uint8Array(data, offset, pixelBytes);
              renderRect(x, y, w, h, pixelData);
              offset += pixelBytes;
            } else {
              console.warn(`  Not enough pixel data: need ${pixelBytes}, have ${data.byteLength - offset}`);
              // This might span multiple WebSocket messages — for now skip
              offset = data.byteLength;
            }
          } else if (encoding === -223) { // DesktopSize pseudo-encoding
            state.width = w;
            state.height = h;
            console.log(`  DesktopSize: ${w}x${h}`);
            const canvas = canvasRef.current?.querySelector('canvas');
            if (canvas) {
              canvas.width = w;
              canvas.height = h;
            }
          } else if (encoding === -239) { // Cursor pseudo-encoding
            // Skip cursor data: w*h*bpp/8 + w*h*bps/8 (mask)
            const pixelBytes = w * h * state.bytesPerPixel;
            const maskBytes = Math.ceil((w * h) / 8);
            offset += pixelBytes + maskBytes;
          } else {
            console.warn(`  Unknown encoding: ${encoding}`);
            // Skip — can't determine size without knowing encoding
            offset = data.byteLength;
          }
        }
        break;
      }
      case 1: { // SetColourMapEntries
        console.log('SetColourMapEntries (ignored)');
        break;
      }
      case 2: { // Bell
        console.log('Bell');
        break;
      }
      case 3: { // ServerCutText
        if (data.byteLength < 8) return;
        const length = view.getUint32(4);
        console.log(`ServerCutText: ${length} bytes`);
        break;
      }
      default: {
        // First message after handshake might be the server name
        // "reMarkable rfb" = 00 00 00 0e + 14 bytes
        if (data.byteLength >= 4) {
          const nameLen = view.getUint32(0);
          if (nameLen > 0 && nameLen < 100 && 4 + nameLen <= data.byteLength) {
            const name = new TextDecoder().decode(new Uint8Array(data, 4, nameLen));
            console.log(`Server name: ${name}`);
            return;
          }
        }
        console.warn(`Unknown RFB message type: ${msgType}, ${data.byteLength} bytes`);
      }
    }
  }, [renderRect]);

  const connectVNC = useCallback(() => {
    if (wsRef.current) {
      wsRef.current.close();
      wsRef.current = null;
    }

    setStatus('connecting');
    setError(null);

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
      // Remove old canvas
      container.innerHTML = '';
      const canvas = document.createElement('canvas');
      const state = rfbStateRef.current;
      canvas.width = state.width;
      canvas.height = state.height;
      canvas.style.maxWidth = '100%';
      canvas.style.maxHeight = '100%';
      canvas.style.imageRendering = 'pixelated';
      canvas.style.background = '#fff';
      container.appendChild(canvas);
    }

    ws.onopen = () => {
      setStatus('connected');
      console.log('VNC WebSocket connected');
    };

    ws.onmessage = (e) => {
      if (e.data instanceof ArrayBuffer) {
        handleMessage(e.data);
      }
    };

    ws.onerror = (e) => {
      setError('WebSocket error');
      setStatus('error');
      console.error('VNC WS error:', e);
    };

    ws.onclose = (e) => {
      setStatus('disconnected');
      if (e.code !== 1000) {
        setError(`Connection closed (code: ${e.code})`);
      }
      wsRef.current = null;
    };
  }, [handleMessage]);

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