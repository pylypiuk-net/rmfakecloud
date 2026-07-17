import React from 'react';
import VNCViewer from './VNCViewer';

export default function ScreenShare() {
  return (
    <div style={{ height: 'calc(100vh - 56px)', display: 'flex', flexDirection: 'column' }}>
      <div style={{
        padding: '12px 20px',
        background: 'white',
        borderBottom: '1px solid #e0e0e0',
        fontSize: '18px',
        fontWeight: 600,
      }}>
        Live View
      </div>
      <VNCViewer />
    </div>
  );
}