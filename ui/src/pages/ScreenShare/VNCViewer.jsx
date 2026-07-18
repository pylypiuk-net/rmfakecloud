import React, { useEffect, useRef, useState, useCallback } from 'react';

const ROOT_URL = '/ui/api';

// Framebuffer viewer for reMarkable screen sharing.
// Data path: restream (-w 1872 -h 1404 -b 1 -s 8 -f :mem:) → LZ4 frame →
//   fbproxy (decompresses LZ4) → raw pixels → WebSocket → rmfakecloud VNCHub → browser
//
// The proxy decompresses LZ4 and sends complete raw frames (2,628,288 bytes each).
// The viewer renders raw 4-bit grayscale pixels directly — no LZ4 decompression needed.
//
// Pixel format: 4-bit grayscale (0-15), 1872 wide × 1404 tall (landscape)
// Rendering: scale 0-15 → 0-255 (×17), rotate 90° CCW → portrait (1404×1872)
// 15 = white (e-ink background), 0 = black (ink) — no inversion needed.

const FB_WIDTH = 1872;
const FB_HEIGHT = 1404;
const FRAME_SIZE = FB_WIDTH * FB_HEIGHT; // 2,628,288 bytes (1 byte per pixel)

export default function VNCViewer({ token, onDisconnect }) {
  const canvasRef = useRef(null);
  const wsRef = useRef(null);
  const bufferRef = useRef(new Uint8Array(0));
  const [status, setStatus] = useState('idle');
  const [frameCount, setFrameCount] = useState(0);
  const [fps, setFps] = useState(0);
  const [bufSize, setBufSize] = useState(0);
  const frameTimesRef = useRef([]);

  const renderFrame = useCallback((frameData) => {
    const canvas = canvasRef.current;
    if (!canvas || frameData.length < FRAME_SIZE) return;

    // Portrait dimensions after 90° CCW rotation
    canvas.width = FB_HEIGHT;  // 1404
    canvas.height = FB_WIDTH;  // 1872

    const ctx = canvas.getContext('2d');
    const imageData = ctx.createImageData(canvas.width, canvas.height);
    const pixels = imageData.data;

    // Source: 1872 wide × 1404 tall, 1 byte per pixel, 4-bit grayscale (0-15)
    // Rotate 90° CCW: dst[px][py] = src[py][FB_WIDTH-1-px]
    // For 90° CCW: dst pixel at (px, py) in portrait (1404×1872):
    //   src_x = py                 (0..1871)
    //   src_y = FB_HEIGHT-1 - px   (0..1403)

    for (let py = 0; py < canvas.height; py++) {
      for (let px = 0; px < canvas.width; px++) {
        const srcX = py;
        const srcY = FB_HEIGHT - 1 - px;
        const srcIdx = srcY * FB_WIDTH + srcX;
        // Scale 4-bit (0-15) to 8-bit (0-255): multiply by 17
        const val = frameData[srcIdx] * 17;
        const dstIdx = (py * canvas.width + px) * 4;
        pixels[dstIdx] = val;     // R
        pixels[dstIdx + 1] = val; // G
        pixels[dstIdx + 2] = val; // B
        pixels[dstIdx + 3] = 255; // A
      }
    }

    ctx.putImageData(imageData, 0, 0);

    // Track FPS
    const now = performance.now();
    frameTimesRef.current.push(now);
    if (frameTimesRef.current.length > 30) {
      frameTimesRef.current.shift();
    }
    if (frameTimesRef.current.length >= 2) {
      const elapsed = frameTimesRef.current[frameTimesRef.current.length - 1] - frameTimesRef.current[0];
      setFps((frameTimesRef.current.length / elapsed * 1000).toFixed(1));
    }
    setFrameCount(c => c + 1);
  }, []);

  const connect = useCallback(() => {
    if (wsRef.current) return;
    setStatus('connecting');
    bufferRef.current = new Uint8Array(0);
    setFrameCount(0);
    setFps(0);

    const wsURL = `${ROOT_URL.replace(/^http/, 'ws')}/screenshare/vnc/stream?token=${encodeURIComponent(token)}`;
    const ws = new WebSocket(wsURL);
    ws.binaryType = 'arraybuffer';
    wsRef.current = ws;

    ws.onopen = () => setStatus('connected');

    ws.onmessage = (event) => {
      const data = new Uint8Array(event.data);

      // Each WebSocket message is a complete raw frame (FRAME_SIZE bytes)
      if (data.length >= FRAME_SIZE) {
        renderFrame(data);
      }
    };

    ws.onerror = (e) => {
      console.error('VNC viewer WS error:', e);
      setStatus('error');
    };
    ws.onclose = () => {
      setStatus('disconnected');
      wsRef.current = null;
    };
  }, [token, renderFrame]);

  const disconnect = useCallback(() => {
    if (wsRef.current) {
      wsRef.current.close();
      wsRef.current = null;
    }
    setStatus('idle');
    bufferRef.current = new Uint8Array(0);
    setBufSize(0);
  }, []);

  useEffect(() => {
    return () => {
      if (wsRef.current) wsRef.current.close();
    };
  }, []);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', gap: '12px' }}>
      <div style={{ display: 'flex', gap: '16px', alignItems: 'center' }}>
        {status !== 'connected' && (
          <button
            onClick={connect}
            disabled={status === 'connecting'}
            style={{
              padding: '8px 16px',
              cursor: status === 'connecting' ? 'wait' : 'pointer',
              background: status === 'connecting' ? '#ccc' : '#1976d2',
              color: 'white',
              border: 'none',
              borderRadius: '4px',
              fontSize: '14px',
            }}
          >
            {status === 'connecting' ? 'Connecting...' : 'Connect'}
          </button>
        )}
        {status === 'connected' && (
          <button
            onClick={disconnect}
            style={{
              padding: '8px 16px',
              cursor: 'pointer',
              background: '#d32f2f',
              color: 'white',
              border: 'none',
              borderRadius: '4px',
              fontSize: '14px',
            }}
          >
            Disconnect
          </button>
        )}
        <span style={{ fontSize: '13px', color: '#666' }}>
          Status: {status} | Frames: {frameCount} | FPS: {fps} | Buffer: {(bufSize / 1024).toFixed(0)}KB
        </span>
      </div>
      <canvas
        ref={canvasRef}
        style={{
          maxWidth: '100%',
          maxHeight: '75vh',
          border: status === 'connected' ? '1px solid #ccc' : '1px dashed #ccc',
          background: '#f5f5f5',
          imageRendering: 'auto',
        }}
      />
      {status === 'idle' && (
        <p style={{ color: '#999', fontSize: '14px' }}>
          Click Connect to start viewing the reMarkable screen.
        </p>
      )}
    </div>
  );
}