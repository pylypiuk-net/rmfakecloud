import React, { useState, useEffect, useRef } from "react";
import { NavDropdown } from "react-bootstrap";
import { logout } from "../common/actions";
import { useAuthState } from "../common/useAuthContext";
import { NavLink, useLocation, useHistory, useParams } from "react-router-dom";
import apiservice from "../services/api.service";
import DocumentTree from "../pages/Documents/Tree";
import { BsSearch } from "react-icons/bs";
import { toast } from "react-toastify";

const Sidebar = () => {
  const { state: { user }, dispatch } = useAuthState();
  const location = useLocation();
  const history = useHistory();
  const { itemId } = useParams();

  // Document tree state (only loaded when on /documents)
  const [entries, setEntries] = useState([]);
  const [selected, setSelected] = useState(null);
  const [term, setTerm] = useState("");
  const [showSearch, setShowSearch] = useState(false);
  const [counter, setCounter] = useState(0);
  const [initialSelectionSet, setInitialSelectionSet] = useState(false);
  const treeRef = useRef(null);
  const lastSelectedId = useRef(null);

  function handleLogout(e) {
    e.preventDefault();
    logout(dispatch);
  }

  function isAdmin() {
    return user && user.Roles && user.Roles[0] === "Admin";
  }

  function isActive(path) {
    if (path === "/") return location.pathname === "/";
    return location.pathname.startsWith(path);
  }

  const isDocumentsPage = location.pathname.startsWith("/documents");

  // Load documents when on documents page
  useEffect(() => {
    if (!isDocumentsPage || !user) return;
    const loadDocs = async () => {
      try {
        const { Trash, Entries } = await apiservice.listDocument();
        const root = {
          id: "root",
          name: "My Files",
          isFolder: true,
          icon: "device",
          children: Entries,
        };
        const trash = {
          id: "trash",
          name: "Trash",
          isFolder: true,
          icon: "trash",
          children: Trash,
        };
        setEntries([root, trash]);
      } catch (e) {
        toast.error(e);
      }
    };
    loadDocs();
  }, [isDocumentsPage, counter, user]);

  // Track last selected
  useEffect(() => {
    lastSelectedId.current = selected?.id || null;
  }, [selected]);

  // Auto-select first item
  useEffect(() => {
    if (
      !initialSelectionSet &&
      !itemId &&
      selected === null &&
      treeRef.current &&
      treeRef.current.root &&
      treeRef.current.root.children[0]
    ) {
      setSelected(treeRef.current.root.children[0]);
      setInitialSelectionSet(true);
    }
  }, [entries, selected, initialSelectionSet, itemId]);

  // Handle tree selection
  const onSelect = (node) => {
    setSelected(node);
    if (typeof node?.toggle === "function") {
      node.toggle();
    }
    if (node && node.id) {
      if (node.id === "root" || node.id === "trash") {
        history.push("/documents");
      } else {
        history.push(`/documents/${node.id}`);
      }
    }
  };

  // Find item in entries by ID
  const findItemInEntries = (entries, targetId, parent = null) => {
    for (const entry of entries) {
      if (entry.id === targetId) return { item: entry, parent };
      if (entry.children && entry.children.length > 0) {
        const found = findItemInEntries(entry.children, targetId, entry);
        if (found) return found;
      }
    }
    return null;
  };

  const buildParentChain = (parentItem) => {
    if (!parentItem) return null;
    const parentNode = {
      id: parentItem.id,
      data: parentItem,
      isLeaf: !parentItem.isFolder,
      isRoot: parentItem.id === "root" || parentItem.id === "trash",
      toggle: () => {},
    };
    if (parentItem.id !== "root" && parentItem.id !== "trash") {
      const grandparentResult = findItemInEntries(entries, parentItem.id);
      if (grandparentResult && grandparentResult.parent) {
        parentNode.parent = buildParentChain(grandparentResult.parent);
      }
    } else {
      parentNode.parent = {
        id: "__REACT_ARBORIST_INTERNAL_ROOT__",
        data: { id: "__REACT_ARBORIST_INTERNAL_ROOT__", name: "" },
        isLeaf: false,
        parent: null,
      };
    }
    return parentNode;
  };

  // Restore selection from URL
  useEffect(() => {
    if (!entries.length || !itemId || initialSelectionSet) return;
    const result = findItemInEntries(entries, itemId);
    if (!result) {
      toast.warning(`Item not found, returning to root`);
      history.push("/documents");
      return;
    }
    const { item: foundItem, parent: parentItem } = result;
    const pseudoNode = {
      id: foundItem.id,
      data: foundItem,
      isLeaf: !foundItem.isFolder,
      children: (foundItem.children || []).map(child => ({
        id: child.id,
        data: child,
        isLeaf: !child.isFolder,
      })),
      parent: parentItem ? buildParentChain(parentItem) : null,
      isRoot: foundItem.id === "root" || foundItem.id === "trash",
    };
    setSelected(pseudoNode);
    setInitialSelectionSet(true);
    if (treeRef.current && typeof treeRef.current.openParents === "function") {
      setTimeout(() => {
        if (treeRef.current && typeof treeRef.current.openParents === "function") {
          treeRef.current.openParents(itemId);
        }
      }, 100);
    }
  }, [entries, itemId, initialSelectionSet]);

  // Expose selected to Documents page via window (hacky but works for now)
  useEffect(() => {
    if (isDocumentsPage) {
      window.__rmSelectedNode = selected;
      window.__rmOnTreeSelect = onSelect;
      window.__rmUpdateCounter = () => setCounter(prev => prev + 1);
    }
  }, [selected, isDocumentsPage]);

  if (!user) return null;

  return (
    <aside className="sidebar">
      <div className="sidebar-header">
        <h1>rmfakecloud</h1>
        <span className="subtitle">reMarkable Cloud</span>
      </div>
      <nav className="sidebar-nav">
        <NavLink to="/" className={`sidebar-nav-item ${isActive("/") ? "active" : ""}`}>
          <i className="fas fa-home"></i><span>Home</span>
        </NavLink>
        <NavLink to="/documents" className={`sidebar-nav-item ${isActive("/documents") ? "active" : ""}`}>
          <i className="fas fa-file-alt"></i><span>Documents</span>
        </NavLink>

        {/* Inline document tree when on documents page */}
        {isDocumentsPage && (
          <div className="sidebar-tree-container">
            {/* Search toggle */}
            <div className="sidebar-tree-header">
              <span style={{fontSize: "11px", color: "#6b7280", textTransform: "uppercase", fontWeight: 600}}>{user.UserID}</span>
              <button
                onClick={() => { setShowSearch(!showSearch); setTerm(""); }}
                style={{background: "none", border: "none", color: "#6b7280", cursor: "pointer", padding: "2px 6px"}}
              >
                <BsSearch size={12} />
              </button>
            </div>

            {showSearch && (
              <input
                autoFocus
                type="text"
                value={term}
                onChange={(e) => setTerm(e.target.value)}
                placeholder="Search..."
                style={{
                  width: "100%",
                  background: "#111827",
                  border: "1px solid #374151",
                  borderRadius: "4px",
                  color: "#e5e7eb",
                  fontSize: "12px",
                  padding: "4px 8px",
                  marginBottom: "4px",
                }}
              />
            )}

            <div className="sidebar-tree" style={{flex: 1, overflow: "auto"}}>
              <DocumentTree
                selection={selected}
                onSelect={onSelect}
                treeRef={treeRef}
                term={term}
                entries={entries}
                height={600}
              />
            </div>
          </div>
        )}

        <NavLink to="/integrations" className={`sidebar-nav-item ${isActive("/integrations") ? "active" : ""}`}>
          <i className="fas fa-plug"></i><span>Integrations</span>
        </NavLink>
        <NavLink to="/connect" className={`sidebar-nav-item ${isActive("/connect") ? "active" : ""}`}>
          <i className="fas fa-link"></i><span>Connect</span>
        </NavLink>
        <NavLink to="/screenshare" className={`sidebar-nav-item ${isActive("/screenshare") ? "active" : ""}`}>
          <i className="fas fa-desktop"></i><span>Screen Share</span>
        </NavLink>
        {isAdmin() && (
          <NavLink to="/admin" className={`sidebar-nav-item ${isActive("/admin") ? "active" : ""}`}>
            <i className="fas fa-cog"></i><span>Admin</span>
          </NavLink>
        )}
      </nav>
      <div className="sidebar-footer">
        <NavDropdown
          id="userMenu"
          title={
            <div className="sidebar-user">
              <div className="sidebar-user-avatar">
                <i className="fas fa-user"></i>
              </div>
              <div>
                <div className="sidebar-user-name">{user.UserID}</div>
                <div className="sidebar-user-role">{isAdmin() ? "Admin" : "User"}</div>
              </div>
            </div>
          }
          align="end"
        >
          <NavDropdown.Item href="/profile">Profile</NavDropdown.Item>
          <NavDropdown.Divider />
          <NavDropdown.Item onClick={handleLogout}>Log out</NavDropdown.Item>
        </NavDropdown>
      </div>
    </aside>
  );
};

export default Sidebar;