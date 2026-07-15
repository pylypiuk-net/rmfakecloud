import React, { useState, useEffect } from "react";
import { useHistory, useLocation } from "react-router-dom";
import { Button, Form } from "react-bootstrap";

import { useAuthState } from "../../common/useAuthContext";
import { loginUser } from "../../common/actions";

const Login = () => {
  let history = useHistory();
  let location = useLocation();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [oidcEnabled, setOidcEnabled] = useState(null); // null = loading, true/false = result
  const [oidcError, setOidcError] = useState("");

  const { state, dispatch } = useAuthState();
  const { errorMessage, loading } = state;

  // Check for OIDC error params in URL
  useEffect(() => {
    const params = new URLSearchParams(location.search);
    const error = params.get("error");
    if (error) {
      setOidcError(decodeURIComponent(error));
    }
  }, [location.search]);

  // Check if OIDC is enabled
  useEffect(() => {
    fetch("/ui/api/oidc/status")
      .then((r) => r.json())
      .then((data) => {
        setOidcEnabled(data.enabled);
      })
      .catch(() => {
        setOidcEnabled(false);
      });
  }, []);

  const handleLogin = async (e) => {
    e.preventDefault();
    let payload = { email: username, password };
    try {
      await loginUser(dispatch, payload);
      history.push("/documents");
    } catch (error) {
      console.log(error);
    }
  };

  const handleSSO = () => {
    window.location.href = "/ui/api/oidc/login";
  };

  const showSSO = oidcEnabled === true;
  const showLocal = oidcEnabled === false || oidcEnabled === null; // show local if OIDC off or still loading
  const showDivider = showSSO && showLocal;

  return (
    <div className="login-container">
      <div className="login-card">
        <div className="login-header">
          <div className="login-icon">
            <i className="fas fa-cloud"></i>
          </div>
          <h1 className="login-title">rmfakecloud</h1>
          <p className="login-subtitle">Sign in to continue</p>
        </div>

        {/* Error messages */}
        {oidcError && (
          <div className="login-error visible">
            {oidcError === "oidc_disabled" && "SSO is not enabled."}
            {oidcError === "no_code" && "Authentication failed: no code received."}
            {oidcError === "invalid_state" && "Authentication failed: state mismatch. Please try again."}
            {oidcError === "no_verifier" && "Authentication session expired. Please try again."}
            {oidcError === "token_exchange_failed" && "SSO token exchange failed. Please try again."}
            {oidcError === "userinfo_failed" && "Failed to get user info from SSO provider."}
            {oidcError === "no_email" && "SSO provider did not return an email address."}
            {oidcError === "user_not_found" && "User not found. Contact an administrator."}
            {oidcError === "create_failed" && "Failed to create user account."}
            {oidcError === "register_failed" && "Failed to register new user."}
            {oidcError === "jwt_failed" && "Failed to create session. Please try again."}
            {!["oidc_disabled","no_code","invalid_state","no_verifier","token_exchange_failed","userinfo_failed","no_email","user_not_found","create_failed","register_failed","jwt_failed"].includes(oidcError) && `SSO error: ${oidcError}`}
          </div>
        )}
        {errorMessage && (
          <div className="login-error visible">{errorMessage}</div>
        )}

        {/* Loading state */}
        {oidcEnabled === null && (
          <div className="loading-spinner">
            <span className="spinner-icon">⣾</span> Checking authentication options...
          </div>
        )}

        {/* SSO button */}
        {showSSO && (
          <div style={{marginBottom: "16px"}}>
            <Button
              className="btn-primary btn-full"
              onClick={handleSSO}
              disabled={loading}
            >
              <i className="fas fa-shield-alt" style={{marginRight: "8px"}}></i>
              Sign in with SSO
            </Button>
          </div>
        )}

        {/* Divider */}
        {showDivider && (
          <div className="divider">
            <span>Or continue with</span>
          </div>
        )}

        {/* Local login form */}
        {showLocal && (
          <Form onSubmit={handleLogin}>
            <Form.Group className="mb-3">
              <Form.Label htmlFor="username" style={{color: "#9ca3af", fontSize: "14px", fontWeight: 500}}>Username</Form.Label>
              <Form.Control
                id="username"
                value={username}
                autoFocus
                onChange={(e) => setUsername(e.target.value)}
                disabled={loading}
                placeholder="Username"
                autoComplete="username"
                style={{
                  background: "#111827",
                  border: "1px solid #374151",
                  borderRadius: "6px",
                  color: "#e5e7eb",
                  fontSize: "14px",
                }}
              />
            </Form.Group>

            <Form.Group className="mb-3">
              <Form.Label htmlFor="password" style={{color: "#9ca3af", fontSize: "14px", fontWeight: 500}}>Password</Form.Label>
              <Form.Control
                type="password"
                id="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                disabled={loading}
                placeholder="Password"
                autoComplete="current-password"
                style={{
                  background: "#111827",
                  border: "1px solid #374151",
                  borderRadius: "6px",
                  color: "#e5e7eb",
                  fontSize: "14px",
                }}
              />
            </Form.Group>

            <Button type="submit" className="btn-primary btn-full" disabled={loading}>
              <i className="fas fa-sign-in-alt" style={{marginRight: "8px"}}></i>
              Sign In
            </Button>
          </Form>
        )}
      </div>
    </div>
  );
};

export default Login;