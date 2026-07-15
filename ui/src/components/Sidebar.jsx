import React from "react";
import { NavDropdown } from "react-bootstrap";
import { logout } from "../common/actions";
import { useAuthState } from "../common/useAuthContext";
import { NavLink, useLocation } from "react-router-dom";

const Sidebar = () => {
  const { state: { user }, dispatch } = useAuthState();
  const location = useLocation();

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