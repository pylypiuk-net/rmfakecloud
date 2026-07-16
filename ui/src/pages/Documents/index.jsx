import apiservice from "../../services/api.service"
import { useEffect, useState } from "react";
import { useParams, useHistory } from "react-router-dom";
import { Container } from "react-bootstrap";
import File from "./File";
import Folder from "./Folder";
import { useAuthState } from "../../common/useAuthContext";

export default function DocumentList() {
  const [selected, setSelected] = useState(null);
  const [counter, setCounter] = useState(0);
  const { itemId } = useParams();
  const history = useHistory();
  const { state: { user } } = useAuthState();

  // Poll for selection from sidebar (set via window.__rmSelectedNode)
  useEffect(() => {
    const interval = setInterval(() => {
      if (window.__rmSelectedNode !== selected) {
        setSelected(window.__rmSelectedNode);
      }
    }, 100);
    return () => clearInterval(interval);
  }, [selected]);

  const onSelect = (node) => {
    if (window.__rmOnTreeSelect) {
      window.__rmOnTreeSelect(node);
    }
  };

  const onUpdate = () => {
    setCounter(prev => prev + 1);
    if (window.__rmUpdateCounter) {
      window.__rmUpdateCounter();
    }
  };

  return (
    <Container fluid style={{
      height: "100%",
      display: "flex",
      flexDirection: "column",
      overflow: "hidden",
      padding: "0",
    }}>
      <div style={{flex: "1 1 auto", minHeight: 0, overflow: "auto"}}>
        {selected && selected.isLeaf && <File file={selected} onSelect={onSelect} />}
        {selected && !selected.isLeaf && (
          <Folder
            selection={selected}
            onSelect={onSelect}
            onUpdate={onUpdate}
            counter={counter}
          />
        )}
        {!selected && (
          <div style={{
            display: "flex",
            alignItems: "center",
            justifyContent: "center",
            height: "100%",
            color: "#6b7280",
            fontSize: "14px",
          }}>
            Select a document from the sidebar
          </div>
        )}
      </div>
    </Container>
  );
}