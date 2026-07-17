import React, { useEffect, useRef, useState, useCallback } from 'react';
import lz4 from 'lz4js';

const ROOT_URL = '/ui/api';

// Framebuffer viewer for reMarkable screen sharing.
// The proxy (fbproxy) runs `restream` on the tablet which captures xochitl's
// framebuffer from process memory (ptrace) and sends LZ4-compressed BGRA
// frames over WebSocket via the VNCHub.
//
// Format: 1872×1404 BGRA (4bpp), LZ4 compressed, rotate 180° (transpose=2)
// E-ink: 0x00000000 = white, 0xFFFFFFFF = black → invert
//
// Protocol:
//   - All WS messages are binary (VNCHub only forwards binary)
//   - Messages are LZ4-compressed chunks of BGRA pixel data
//   - Accumulate compressed data, decompress LZ4 frames, render BGRA→RGBA
export default function VNCViewer() {
  const canvasRef = useRef(null);
  const wsRef = useRef(null);
  const compressedBufRef = useRef(null);
  const [status, setStatus] = useState('disconnected');
  const [error, setError] = useState(null);
  const [stats, setStats] = useState({ frames: 0, bytes: 0 });

  // reMarkable 2 firmware ≥3.24: landscape BGRA, rotate 180°
  const FB_WIDTH = 1872;
  const FB_HEIGHT = 1404;
  const BPP = 4;
  const FRAME_SIZE = FB_WIDTH * FB_HEIGHT * BPP;
  const LZ4_MAGIC = 0x184d2204;

  // Render BGRA data to canvas with 180° rotation and e-ink inversion
  const renderFrame = useCallback((pixelData) => {
    const container = canvasRef.current;
    if (!container) return;
    const canvas = container.querySelector('canvas');
    if (!canvas) return;
    const ctx = canvas.getContext('2d');

    // Canvas is portrait (1404×1872) — the tablet's physical orientation
    const canvasW = 1404;
    const canvasH = 1872;
    if (canvas.width !== canvasW) canvas.width = canvasW;
    if (canvas.height !== canvasH) canvas.height = canvasH;

    const imgData = ctx.createImageData(canvasW, canvasH);
    const view = new DataView(pixelData.buffer, pixelData.byteOffset, pixelData.byteLength);

    // Source: 1872×1404 BGRA, rotated 180° → destination: 1404×1872
    // transpose=2 means: dst[y][x] = src[height-1-y][width-1-x]
    for (let y = 0; y < canvasH; y++) {
      for (let x = 0; x < canvasW; x++) {
        // Map portrait coords to landscape with 180° rotation
        const srcX = FB_WIDTH - 1 - x;
        const srcY = FB_HEIGHT - 1 - y;
        const srcOffset = (srcY * FB_WIDTH + srcX) * BPP;

        if (srcOffset + 3 >= view.byteLength) continue;

        // BGRA: B, G, R, A
        const b = view.getUint8(srcOffset);
        const g = view.getUint8(srcOffset + 1);
        const r = view.getUint8(srcOffset + 2);

        // E-ink: 0=white, 255=black → invert
        const dstIdx = (y * canvasW + x) * 4;
        imgData.data[dstIdx] = 255 - r;
        imgData.data[dstIdx + 1] = 255 - g;
        imgData.data[dstIdx + 2] = 255 - b;
        imgData.data[dstIdx + 3] = 255;
      }
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
    compressedBufRef.current = new Uint8Array(0);

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
      console.log('[FB] WebSocket connected');
    };

    ws.onmessage = (e) => {
      if (e.data instanceof ArrayBuffer) {
        const newData = new Uint8Array(e.data);

        // Append to compressed buffer
        const old = compressedBufRef.current;
        const combined = new Uint8Array(old.length + newData.length);
        combined.set(old);
        combined.set(newData, old.length);
        compressedBufRef.current = combined;

        // Try to decompress complete LZ4 frames
        let buf = compressedBufRef.current;

        while (buf.length > 8) {
          const view = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
          const magic = view.getUint32(0, true);

          if (magic !== LZ4_MAGIC) {
            // Not LZ4 frame start — skip 1 byte
            buf = buf.slice(1);
            continue;
          }

          // Try to decompress
          try {
            const decompressed = lz4.decompress(buf);
            if (decompressed && decompressed.length >= FRAME_SIZE) {
              // We have at least one complete frame
              const frameData = decompressed.slice(0, FRAME_SIZE);
              renderFrame(frameData);

              setStats(prev => ({
                frames: prev.frames + 1,
                bytes: prev.bytes + decompressed.length,
              }));

              // Consume the compressed data we used
              // lz4.decompress consumes the entire input, so we reset the buffer
              compressedBufRef.current = new Uint8Array(0);
              break;
            } else if (decompressed && decompressed.length > 0) {
              // Partial frame — wait for more data
              break;
            } else {
              buf = buf.slice(1);
              continue;
            }
          } catch (err) {
            // Incomplete LZ4 frame — wait for more data
            break;
          }
        }

        if (buf !== compressedBufRef.current) {
          compressedBufRef.current = buf;
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