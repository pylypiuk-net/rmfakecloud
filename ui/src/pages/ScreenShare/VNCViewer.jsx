import React, { useEffect, useRef, useState, useCallback } from 'react';

const ROOT_URL = '/ui/api';

// Framebuffer viewer for reMarkable screen sharing.
// The proxy (fbproxy) runs `restream` on the tablet which captures xochitl's
// framebuffer from process memory (ptrace) and sends raw RGB565 frames
// over WebSocket via the VNCHub.
//
// Protocol:
//   - All messages are binary (VNCHub only forwards binary)
//   - Messages are chunks of raw RGB565 pixel data
//   - Accumulate until we have a full frame (1404×1872×2 = 5,253,576 bytes)
//   - Render with e-ink color inversion (0x0000=white, 0xFFFF=black)
export default function VNCViewer() {
  const canvasRef = useRef(null);
  const wsRef = useRef(null);
  const frameBufRef = useRef(null);
  const [status, setStatus] = useState('disconnected');
  const [error, setError] = useState(null);
  const [stats, setStats] = useState({ frames: 0, bytes: 0 });

  const WIDTH = 1404;
  const HEIGHT = 1872;
  const BPP = 2;
  const FRAME_SIZE = WIDTH * HEIGHT * BPP;

  // Render RGB565 data to canvas
  const renderFrame = useCallback((pixelData) => {
    const container = canvasRef.current;
    if (!container) return;
    const canvas = container.querySelector('canvas');
    if (!canvas) return;
    const ctx = canvas.getContext('2d');

    const numPixels = WIDTH * HEIGHT;
    const imgData = ctx.createImageData(WIDTH, HEIGHT);
    const view = new DataView(pixelData.buffer, pixelData.byteOffset, pixelData.byteLength);

    for (let i = 0; i < numPixels; i++) {
      const offset = i * 2;
      if (offset + 1 >= view.byteLength) break;
      const pixel = view.getUint16(offset, true); // little-endian
      const r = (pixel >> 11) & 0x1f;
      const g = (pixel >> 5) & 0x3f;
      const b = pixel & 0x1f;
      // E-ink: 0x0000 = white, 0xFFFF = black → invert
      imgData.data[i * 4] = 255 - ((r << 3) | (r >> 2));
      imgData.data[i * 4 + 1] = 255 - ((g << 2) | (g >> 4));
      imgData.data[i * 4 + 2] = 255 - ((b << 3) | (b >> 2));
      imgData.data[i * 4 + 3] = 255;
    }

    ctx.putImageData(imgData, 0, 0);
  }, []);

  const connectVNC = useCallback(() => {
    if (wsRef.current) {
      wsRef.current.close();
      wsRef.current = null;
    }

    setStatus('connecting');
    setError(null);
    setStats({ frames: 0, bytes: 0 });
    frameBufRef.current = new Uint8Array(0);

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
      canvas.width = WIDTH;
      canvas.height = HEIGHT;
      canvas.style.maxWidth = '100%';
      canvas.style.maxHeight = '100%';
      canvas.style.imageRendering = 'pixelated';
      canvas.style.background = '#fff';
      container.appendChild(canvas);
    }

    ws.onopen = () => {
      setStatus('connected');
      console.log('[FB] WebSocket connected');
    };

    ws.onmessage = (e) => {
      if (e.data instanceof ArrayBuffer) {
        const newData = new Uint8Array(e.data);

        // Append to frame buffer
        const old = frameBufRef.current;
        const combined = new Uint8Array(old.length + newData.length);
        combined.set(old);
        combined.set(newData, old.length);
        frameBufRef.current = combined;

        // Check if we have enough data for a complete frame
        while (frameBufRef.current.length >= FRAME_SIZE) {
          // Extract one frame
          const frameData = frameBufRef.current.slice(0, FRAME_SIZE);
          renderFrame(frameData);

          setStats(prev => ({
            frames: prev.frames + 1,
            bytes: prev.bytes + FRAME_SIZE,
          }));

          // Keep remaining data
          frameBufRef.current = frameBufRef.current.slice(FRAME_SIZE);
        }
      }
    };

    ws.onerror = (e) => {
      setError('WebSocket error');
      setStatus('error');
      console.error('[FB] WS error:', e);
    };

    ws.onclose = (e) => {
      setStatus('disconnected');
      if (e.code !== 1000) {
        setError(`Connection closed (code: ${e.code})`);
      }
      wsRef.current = null;
    };
  }, [renderFrame]);

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
          reMarkable Live View
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