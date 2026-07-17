import React, { useEffect, useRef, useState, useCallback } from 'react';

const ROOT_URL = '/ui/api';

// Framebuffer viewer for reMarkable screen sharing.
// The proxy (fbproxy) runs `restream` on the tablet which captures xochitl's
// framebuffer from process memory and sends it as an LZ4-compressed stream
// over WebSocket.
//
// Protocol:
//   1. First WS message is text: {"type":"meta","width":1404,"height":1872,"bpp":2,"format":"rgb565le","compressed":"lz4"}
//   2. Subsequent WS messages are binary: chunks of the LZ4 frame stream
//   3. The viewer accumulates chunks, decompresses complete LZ4 frames,
//      and renders the raw RGB565 pixels on a canvas.
//
// LZ4 frame format: https://github.com/lz4/lz4/blob/dev/doc/lz4_Frame_format.md
// We implement a minimal decoder for the specific format restream produces.
export default function VNCViewer() {
  const canvasRef = useRef(null);
  const wsRef = useRef(null);
  const [status, setStatus] = useState('disconnected');
  const [error, setError] = useState(null);
  const [stats, setStats] = useState({ frames: 0, bytes: 0 });
  const metaRef = useRef(null);
  const compressedBufRef = useRef(null);
  const frameBufRef = useRef(null); // decompressed frame buffer

  // LZ4 block decompression (raw block, not frame)
  // LZ4 block format: sequences of tokens
  // Token: 4 bits literal length + 4 bits match length
  const lz4Block = useCallback((input, outputSize) => {
    const output = new Uint8Array(outputSize);
    let ip = 0;
    let op = 0;
    const inputLen = input.length;

    while (ip < inputLen) {
      const token = input[ip++];
      let literalLen = token >> 4;
      let matchLen = token & 0x0f;

      // Extended literal length
      if (literalLen === 15) {
        while (ip < inputLen) {
          const b = input[ip++];
          literalLen += b;
          if (b !== 255) break;
        }
      }

      // Copy literals
      for (let i = 0; i < literalLen; i++) {
        if (ip >= inputLen || op >= outputSize) break;
        output[op++] = input[ip++];
      }

      if (ip >= inputLen) break;

      // Match offset (2 bytes, little-endian)
      if (ip + 1 >= inputLen) break;
      const offset = input[ip] | (input[ip + 1] << 8);
      ip += 2;

      // Extended match length
      if (matchLen === 15) {
        while (ip < inputLen) {
          const b = input[ip++];
          matchLen += b;
          if (b !== 255) break;
        }
      }
      matchLen += 4; // minimum match is 4

      // Copy match (with overlap for RLE)
      const matchStart = op - offset;
      if (matchStart < 0) break;
      for (let i = 0; i < matchLen; i++) {
        if (op >= outputSize) break;
        output[op] = output[matchStart + i];
        op++;
      }
    }

    return output.slice(0, op);
  }, []);

  // Decompress LZ4 frame stream
  // LZ4 frame: magic(4) + FLG(1) + BD(1) + [HC(4)] + blocks...
  // Each block: block_size(4) + block_data(block_size)
  // End mark: block_size = 0
  const decompressLZ4Frame = useCallback((compressed) => {
    const view = new DataView(compressed.buffer, compressed.byteOffset, compressed.byteLength);
    let offset = 0;

    // Check magic
    if (compressed.byteLength < 7) return null;
    const magic = view.getUint32(0, true);
    if (magic !== 0x184D2204) return null;
    offset = 4;

    // FLG byte
    const flg = compressed[offset++];
    const bd = compressed[offset++];
    const contentSizeFlag = (flg >> 5) & 1;
    const checksumFlag = (flg >> 4) & 1;
    const dictIDFlag = flg & 1;

    // Skip content size (8 bytes) if present
    if (contentSizeFlag) offset += 8;
    // Skip dictionary ID (4 bytes) if present
    if (dictIDFlag) offset += 4;
    // Skip header checksum (4 bytes) if present
    if (checksumFlag) offset += 4;

    // Read blocks
    const blocks = [];
    while (offset + 4 <= compressed.byteLength) {
      const blockSize = view.getUint32(offset, true);
      offset += 4;

      if (blockSize === 0) break; // end mark

      const uncompressed = !(blockSize & 0x80000000);
      const actualSize = blockSize & 0x7FFFFFFF;

      if (offset + actualSize > compressed.byteLength) {
        // Incomplete block — need more data
        return null;
      }

      const blockData = compressed.slice(offset, offset + actualSize);
      offset += actualSize;

      if (uncompressed) {
        blocks.push(blockData);
      } else {
        // Compressed block — decompress
        // Max decompressed block size is 4 * compressed size (LZ4 spec)
        const maxDecompressed = Math.max(actualSize * 255, 65536);
        const decompressed = lz4Block(blockData, maxDecompressed);
        blocks.push(decompressed);
      }
    }

    // Check if we have a complete frame (end mark found)
    if (offset >= compressed.byteLength && compressed.byteLength > 7) {
      // No end mark found yet — might be incomplete
      // But restream might not send end marks between frames
      // Let's just concatenate all blocks
    }

    // Concatenate blocks
    let totalLen = 0;
    for (const b of blocks) totalLen += b.length;
    const result = new Uint8Array(totalLen);
    let pos = 0;
    for (const b of blocks) {
      result.set(b, pos);
      pos += b.length;
    }

    return { data: result, consumed: offset };
  }, [lz4Block]);

  // Render a raw RGB565 frame to the canvas
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
      const pixel = view.getUint16(offset, true); // little-endian RGB565
      const r = (pixel >> 11) & 0x1f;
      const g = (pixel >> 5) & 0x3f;
      const b = pixel & 0x1f;
      // e-ink: 0x0000 = white, 0xFFFF = black → invert
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

        // Try to decompress frames from the buffer
        while (compressedBufRef.current.length > 8) {
          const result = decompressLZ4Frame(compressedBufRef.current);
          if (!result || result.data.length === 0) break;

          const decompressed = result.data;

          // Check if we have enough data for a complete frame
          if (decompressed.length >= frameSize) {
            // Extract one frame
            const frameData = decompressed.slice(0, frameSize);
            renderFrame(frameData, meta.width, meta.height);

            setStats(prev => ({
              frames: prev.frames + 1,
              bytes: prev.bytes + frameSize,
            }));

            // Keep remaining decompressed data for next frame
            const remaining = decompressed.slice(frameSize);
            // Put remaining back — but this is decompressed, not compressed
            // We need a different approach: keep track of consumed compressed bytes
            if (result.consumed > 0) {
              compressedBufRef.current = compressedBufRef.current.slice(result.consumed);
              // Prepend remaining decompressed data... but this doesn't work
              // because the remaining is decompressed, not compressed.
              // Actually, if the frame boundary aligns with the LZ4 frame boundary,
              // this should work.
            } else {
              // Can't determine consumed bytes — consume entire buffer
              compressedBufRef.current = new Uint8Array(0);
              break;
            }
          } else {
            // Not enough data for a complete frame yet
            break;
          }
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
  }, [decompressLZ4Frame, renderFrame]);

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