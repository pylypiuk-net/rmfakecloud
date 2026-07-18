import React, { useEffect, useRef, useState, useCallback } from 'react';
import lz4 from 'lz4js';

const ROOT_URL = '/ui/api';

// Framebuffer viewer for reMarkable screen sharing.
// Data path: restream (-w 1872 -h 1404 -b 1 -s 8 -f :mem:) → LZ4 compressed →
//   fbproxy → WebSocket → rmfakecloud VNCHub → browser → lz4js decompress → canvas
//
// Pixel format: 8-bit grayscale, 1872 wide × 1404 tall (landscape)
// The browser renders it rotated 90° CW to portrait (1404×1872)
// E-ink inversion: 255-value (0=black, 255=white on e-ink; invert for screen)

const FB_WIDTH = 1872;
const FB_HEIGHT = 1404;
const FRAME_SIZE = FB_WIDTH * FB_HEIGHT; // 2,628,288 bytes (1bpp)

export default function VNCViewer({ token, onDisconnect }) {
  const canvasRef = useRef(null);
  const wsRef = useRef(null);
  const bufferRef = useRef(new Uint8Array(0));
  const [status, setStatus] = useState('idle');
  const [frameCount, setFrameCount] = useState(0);
  const [fps, setFps] = useState(0);
  const frameTimesRef = useRef([]);

  const renderFrame = useCallback((frameData) => {
    const canvas = canvasRef.current;
    if (!canvas) return;

    // Portrait dimensions (after 90° CW rotation)
    canvas.width = FB_HEIGHT;  // 1404
    canvas.height = FB_WIDTH;  // 1872

    const ctx = canvas.getContext('2d');
    const imageData = ctx.createImageData(canvas.width, canvas.height);
    const pixels = imageData.data;

    // Source is 1872 (width) × 1404 (height), 1 byte per pixel, grayscale
    // Rotate 90° CW: dst[x][y] = src[y][FB_WIDTH-1-x]
    // dst pixel at (px, py) in portrait (1404×1872):
    //   src_x = FB_WIDTH - 1 - py  (0..1871)
    //   src_y = px                 (0..1403)

    for (let py = 0; py < canvas.height; py++) {
      for (let px = 0; px < canvas.width; px++) {
        const srcX = FB_WIDTH - 1 - py;
        const srcY = px;
        const srcIdx = srcY * FB_WIDTH + srcX;
        const val = 255 - frameData[srcIdx]; // e-ink inversion
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

    const wsURL = `${ROOT_URL.replace(/^http/, 'ws')}/screenshare/vnc/stream?token=${encodeURIComponent(token)}`;
    const ws = new WebSocket(wsURL);
    ws.binaryType = 'arraybuffer';
    wsRef.current = ws;

    ws.onopen = () => setStatus('connected');

    ws.onmessage = (event) => {
      const data = new Uint8Array(event.data);
      // Append to buffer
      const newBuf = new Uint8Array(bufferRef.current.length + data.length);
      newBuf.set(bufferRef.current);
      newBuf.set(data, bufferRef.current.length);
      bufferRef.current = newBuf;

      // Try to decompress complete frames
      while (bufferRef.current.length >= FRAME_SIZE) {
        // LZ4 decompress the frame
        try {
          const decompressed = lz4.decompress(bufferRef.current, FRAME_SIZE);
          if (decompressed.length === FRAME_SIZE) {
            renderFrame(decompressed);
          }
          // Remove consumed bytes (assume one LZ4 frame = one framebuffer frame)
          bufferRef.current = new Uint8Array(0);
        } catch (e) {
          // Not enough data or decompression error — wait for more
          break;
        }
      }
    };

    ws.onerror = () => setStatus('error');
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
          <button onClick={connect} disabled={status === 'connecting'} style={{ padding: '8px 16px' }}>
            {status === 'connecting' ? 'Connecting...' : 'Connect'}
          </button>
        )}
        {status === 'connected' && (
          <button onClick={disconnect} style={{ padding: '8px 16px' }}>Disconnect</button>
        )}
        <span style={{ fontSize: '14px', color: '#666' }}>
          Status: {status} | Frames: {frameCount} | FPS: {fps}
        </span>
      </div>
      <canvas
        ref={canvasRef}
        style={{
          maxWidth: '100%',
          maxHeight: '80vh',
          border: status === 'connected' ? '1px solid #ccc' : '1px dashed #ccc',
          background: '#f5f5f5',
        }}
      />
    </div>
  );
}