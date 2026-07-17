import React, { useEffect, useRef, useState, useCallback } from 'react';
import lz4 from 'lz4js';

const ROOT_URL = '/ui/api';

// Framebuffer viewer for reMarkable screen sharing.
// The proxy (fbproxy) runs `restream` on the tablet which captures xochitl's
// framebuffer from process memory (ptrace) and sends LZ4-compressed RGB565
// frames over WebSocket.
//
// Protocol:
//   1. First WS message is text: {"type":"meta","width":1404,"height":1872,"bpp":2,...}
//   2. Subsequent WS messages are binary: LZ4 frame chunks
//   3. Viewer accumulates chunks, decompresses complete LZ4 frames,
//      and renders RGB565 pixels on a canvas with e-ink color inversion.
export default function VNCViewer() {
  const canvasRef = useRef(null);
  const wsRef = useRef(null);
  const [status, setStatus] = useState('disconnected');
  const [error, setError] = useState(null);
  const [stats, setStats] = useState({ frames: 0, bytes: 0 });
  const metaRef = useRef(null);
  const compressedBufRef = useRef(null);

  // Render a raw RGB565 frame to the canvas with e-ink color inversion
  const renderFrame = useCallback((pixelData, width, height) => {
    const container = canvasRef.current;
    if (!container) return;
    const canvas = container.querySelector('canvas');
    if (!canvas) return;
    const ctx = canvas.getContext('2d');

    if (canvas.width !== width) canvas.width = width;
    if (canvas.height !== height) canvas.height = height;

    const numPixels = width * height;
    const imgData = ctx.createImageData(width, height);
    const view = new DataView(pixelData.buffer, pixelData.byteOffset, pixelData.byteLength);

    for (let i = 0; i < numPixels; i++) {
      const offset = i * 2;
      if (offset + 1 >= view.byteLength) break;
      // Little-endian RGB565
      const pixel = view.getUint16(offset, true);
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
    metaRef.current = null;
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
      if (typeof e.data === 'string') {
        // Metadata message
        try {
          const meta = JSON.parse(e.data);
          if (meta.type === 'meta') {
            metaRef.current = meta;
            console.log('[FB] Metadata:', meta);
            const container = canvasRef.current;
            if (container) {
              const canvas = container.querySelector('canvas');
              if (canvas) {
                canvas.width = meta.width;
                canvas.height = meta.height;
              }
            }
          }
        } catch (err) {
          console.error('[FB] Parse metadata:', err);
        }
        return;
      }

      if (e.data instanceof ArrayBuffer) {
        const newData = new Uint8Array(e.data);

        // Append to compressed buffer
        const old = compressedBufRef.current;
        const combined = new Uint8Array(old.length + newData.length);
        combined.set(old);
        combined.set(newData, old.length);
        compressedBufRef.current = combined;

        const meta = metaRef.current || { width: 1404, height: 1872, bpp: 2 };
        const frameSize = meta.width * meta.height * meta.bpp;

        // Try to decompress LZ4 frames from the buffer
        // restream sends a continuous stream of LZ4 frames
        // Each LZ4 frame starts with magic 0x04224D18
        let buf = compressedBufRef.current;

        while (buf.length > 8) {
          // Find LZ4 frame magic number
          const view = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
          const magic = view.getUint32(0, true);

          if (magic !== 0x184D2204) {
            // Not a valid LZ4 frame — skip 1 byte and try again
            buf = buf.slice(1);
            continue;
          }

          // Try to decompress
          try {
            const decompressed = lz4.decompress(buf);
            if (decompressed && decompressed.length >= frameSize) {
              // We have at least one complete frame
              const frameData = decompressed.slice(0, frameSize);
              renderFrame(frameData, meta.width, meta.height);

              setStats(prev => ({
                frames: prev.frames + 1,
                bytes: prev.bytes + frameSize,
              }));

              // Keep the rest for next frame
              const remaining = decompressed.slice(frameSize);
              if (remaining.length > 0) {
                // We have leftover decompressed data — but it's decompressed, not compressed
                // This means multiple frames were in one LZ4 stream
                // For now, just discard and wait for the next compressed frame
              }

              // Consume the entire buffer (restream sends one frame at a time)
              compressedBufRef.current = new Uint8Array(0);
              break;
            } else if (decompressed && decompressed.length > 0) {
              // Partial frame — wait for more data
              break;
            } else {
              // Decompression failed — skip 1 byte
              buf = buf.slice(1);
              continue;
            }
          } catch (err) {
            // Incomplete frame — wait for more data
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