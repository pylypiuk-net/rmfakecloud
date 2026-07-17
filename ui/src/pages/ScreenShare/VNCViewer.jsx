import React, { useEffect, useRef, useState, useCallback } from 'react';

const ROOT_URL = '/ui/api';

// Framebuffer viewer for reMarkable screen sharing.
// The proxy (fbproxy) reads xochitl's framebuffer from process memory,
// converts BGRA→grayscale, rotates to portrait, and sends raw 8-bit
// grayscale frames (1404×1872) over WebSocket.
//
// Protocol:
//   1. First WS message is text: {"type":"meta","width":1404,"height":1872,"bpp":1,"format":"gray8"}
//   2. Subsequent WS messages are binary: raw grayscale frames (1404×1872 = 2,629,488 bytes)
export default function VNCViewer() {
  const canvasRef = useRef(null);
  const wsRef = useRef(null);
  const [status, setStatus] = useState('disconnected');
  const [error, setError] = useState(null);
  const [stats, setStats] = useState({ frames: 0, bytes: 0 });
  const metaRef = useRef(null);

  const connectVNC = useCallback(() => {
    if (wsRef.current) {
      wsRef.current.close();
      wsRef.current = null;
    }

    setStatus('connecting');
    setError(null);
    setStats({ frames: 0, bytes: 0 });
    metaRef.current = null;

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
        const meta = metaRef.current || { width: 1404, height: 1872, bpp: 1 };
        const frameData = new Uint8Array(e.data);
        const expectedSize = meta.width * meta.height * meta.bpp;

        if (frameData.length < expectedSize) {
          console.warn(`[FB] Incomplete frame: ${frameData.length} / ${expectedSize}`);
          return;
        }

        // Render grayscale frame to canvas
        const container = canvasRef.current;
        if (!container) return;
        const canvas = container.querySelector('canvas');
        if (!canvas) return;
        const ctx = canvas.getContext('2d');

        const imgData = ctx.createImageData(meta.width, meta.height);
        // E-ink: 0=black, 255=white (no inversion needed for direct memory read)
        // The fbproxy reads the R channel from BGRA, which is already grayscale
        for (let i = 0; i < meta.width * meta.height; i++) {
          const gray = frameData[i];
          imgData.data[i * 4] = gray;
          imgData.data[i * 4 + 1] = gray;
          imgData.data[i * 4 + 2] = gray;
          imgData.data[i * 4 + 3] = 255;
        }
        ctx.putImageData(imgData, 0, 0);

        setStats(prev => ({
          frames: prev.frames + 1,
          bytes: prev.bytes + frameData.length,
        }));
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
  }, []);

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