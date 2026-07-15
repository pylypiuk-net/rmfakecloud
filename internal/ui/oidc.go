package ui

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ddvk/rmfakecloud/internal/common"
	"github.com/ddvk/rmfakecloud/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v4"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// PKCE state stored in a short-lived cookie to correlate the callback.
// We don't use a session store — the state cookie is enough for CSRF.
const (
	oidcStateCookie   = ".rmoidcstate"
	oidcVerifierCookie = ".rmoidcverifier"
	oidcRedirectCookie = ".rmoidcredirect"
)

// generateState generates a random hex state string.
func generateState() string {
	b := make([]byte, 32)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// generateCodeVerifier generates a PKCE code verifier (base64url, 43+ chars).
func generateCodeVerifier() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// generateCodeChallenge generates the S256 PKCE code challenge from a verifier.
func generateCodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// oidcStatusHandler returns whether OIDC is enabled (for the login page).
func (app *ReactAppWrapper) oidcStatusHandler(c *gin.Context) {
	enabled := app.cfg.OIDC != nil && app.cfg.OIDC.Enabled
	c.JSON(http.StatusOK, gin.H{"enabled": enabled})
}

// oidcLoginHandler redirects to the Authentik OIDC provider with PKCE.
func (app *ReactAppWrapper) oidcLoginHandler(c *gin.Context) {
	if app.cfg.OIDC == nil || !app.cfg.OIDC.Enabled {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "OIDC not configured"})
		return
	}

	oidc := app.cfg.OIDC
	state := generateState()
	codeVerifier := generateCodeVerifier()
	codeChallenge := generateCodeChallenge(codeVerifier)

	// Store state + verifier in short-lived cookies (5 min)
	secure := app.cfg.HTTPSCookie
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(oidcStateCookie, state, 300, "/", "", secure, true)
	c.SetCookie(oidcVerifierCookie, codeVerifier, 300, "/", "", secure, true)

	// Optional redirect target after login
	returnTo := c.Query("returnTo")
	if returnTo == "" {
		returnTo = "/"
	}
	c.SetCookie(oidcRedirectCookie, returnTo, 300, "/", "", secure, true)

	authURL := oidc.Issuer + "/application/o/authorize/?client_id=" + oidc.ClientID +
		"&redirect_uri=" + url.QueryEscape(oidc.CallbackURL) +
		"&response_type=code&scope=openid+profile+email&state=" + state +
		"&code_challenge=" + codeChallenge +
		"&code_challenge_method=S256"

	log.Info("[oidc] redirecting to: ", oidc.Issuer, "/application/o/authorize/")
	c.Redirect(http.StatusFound, authURL)
}

// oidcCallbackHandler handles the OIDC callback: token exchange, user
// lookup/linking, and JWT cookie issuance.
func (app *ReactAppWrapper) oidcCallbackHandler(c *gin.Context) {
	if app.cfg.OIDC == nil || !app.cfg.OIDC.Enabled {
		c.Redirect(http.StatusFound, "/login?error=oidc_disabled")
		return
	}

	oidc := app.cfg.OIDC

	code := c.Query("code")
	state := c.Query("state")
	errorParam := c.Query("error")

	if errorParam != "" {
		log.Warn("[oidc] provider error: ", errorParam)
		c.Redirect(http.StatusFound, "/login?error="+errorParam)
		return
	}
	if code == "" {
		c.Redirect(http.StatusFound, "/login?error=no_code")
		return
	}

	// Validate state from cookie
	savedState, err := c.Cookie(oidcStateCookie)
	if err != nil || savedState != state {
		log.Warn("[oidc] state mismatch")
		c.Redirect(http.StatusFound, "/login?error=invalid_state")
		return
	}

	// Get code verifier from cookie
	codeVerifier, err := c.Cookie(oidcVerifierCookie)
	if err != nil {
		log.Warn("[oidc] no verifier cookie")
		c.Redirect(http.StatusFound, "/login?error=no_verifier")
		return
	}

	// Clear OIDC cookies
	secure := app.cfg.HTTPSCookie
	c.SetCookie(oidcStateCookie, "", -1, "/", "", secure, true)
	c.SetCookie(oidcVerifierCookie, "", -1, "/", "", secure, true)

	// Exchange code for tokens
	tokenURL := oidc.Issuer + "/application/o/token/"
	data := url.Values{}
	data.Set("grant_type", "authorization_code")
	data.Set("code", code)
	data.Set("redirect_uri", oidc.CallbackURL)
	data.Set("client_id", oidc.ClientID)
	data.Set("client_secret", oidc.ClientSecret)
	data.Set("code_verifier", codeVerifier)

	req, _ := http.NewRequest("POST", tokenURL, strings.NewReader(data.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Error("[oidc] token exchange HTTP error: ", err)
		c.Redirect(http.StatusFound, "/login?error=token_exchange_failed")
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		log.Error("[oidc] token exchange failed, status: ", resp.StatusCode)
		c.Redirect(http.StatusFound, "/login?error=token_exchange_failed")
		return
	}

	var tokenRes struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
		Error       string `json:"error"`
	}
	json.Unmarshal(body, &tokenRes)
	if tokenRes.Error != "" {
		log.Error("[oidc] token response error: ", tokenRes.Error)
		c.Redirect(http.StatusFound, "/login?error="+tokenRes.Error)
		return
	}

	// Get user info
	userInfoURL := oidc.Issuer + "/application/o/userinfo/"
	req, _ = http.NewRequest("GET", userInfoURL, nil)
	req.Header.Set("Authorization", "Bearer "+tokenRes.AccessToken)

	userResp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Error("[oidc] userinfo request failed: ", err)
		c.Redirect(http.StatusFound, "/login?error=userinfo_failed")
		return
	}
	defer userResp.Body.Close()

	userBody, _ := io.ReadAll(userResp.Body)
	var userInfo struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
		Name             string `json:"name"`
		PreferredUsername string `json:"preferred_username"`
	}
	json.Unmarshal(userBody, &userInfo)

	log.Info("[oidc] userinfo: sub=", userInfo.Sub, " email=", userInfo.Email, " name=", userInfo.Name, " preferred_username=", userInfo.PreferredUsername)

	// Link to existing user by email (sanitized, same as model.NewUser)
	userID := sanitizeEmail(userInfo.Email)
	if userID == "" {
		log.Error("[oidc] no email in userinfo")
		c.Redirect(http.StatusFound, "/login?error=no_email")
		return
	}

	user, err := app.userStorer.GetUser(userID)

	// Fallback: if email lookup failed, try matching by Authentik username.
	// This handles users created with a bare username (e.g. "ypyly") rather than an email.
	// Try "preferred_username" first (standard OIDC claim), then "name" (Authentik username).
	if (err != nil || user == nil) {
		fallbackIDs := []string{userInfo.PreferredUsername, userInfo.Name}
		for _, fid := range fallbackIDs {
			if fid == "" {
				continue
			}
			log.Info("[oidc] email lookup failed, trying username fallback: ", fid)
			user, err = app.userStorer.GetUser(fid)
			if err == nil && user != nil {
				log.Info("[oidc] user found via username fallback: ", fid)
				break
			}
		}
	}

	if err != nil || user == nil {
		// User not found
		if oidc.AutoCreate {
			log.Info("[oidc] auto-creating user: ", userID)
			user, err = model.NewUser(userID, uuid.NewString()) // random password, OIDC-only
			if err != nil {
				log.Error("[oidc] failed to create user: ", err)
				c.Redirect(http.StatusFound, "/login?error=create_failed")
				return
			}
			user.EmailVerified = true
			if userInfo.Name != "" {
				user.Name = userInfo.Name
			}
			err = app.userStorer.RegisterUser(user)
			if err != nil {
				log.Error("[oidc] failed to register user: ", err)
				c.Redirect(http.StatusFound, "/login?error=register_failed")
				return
			}
		} else {
			log.Warn("[oidc] user not found and auto-create disabled: ", userID)
			c.Redirect(http.StatusFound, "/login?error=user_not_found")
			return
		}
	}

	// Issue the same WebUserClaims JWT as local login
	scopes := ""
	if user.Sync15 {
		scopes = isSync15Key
	}
	expiresAfter := 24 * time.Hour
	expires := time.Now().Add(expiresAfter)
	claims := &WebUserClaims{
		UserID:    user.ID,
		BrowserID: uuid.NewString(),
		Email:     user.Email,
		Scopes:    scopes,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expires),
			Issuer:    "rmFake WEB",
			Audience:  []string{WebUsage},
		},
	}
	if user.IsAdmin {
		claims.Roles = []string{AdminRole}
	} else {
		claims.Roles = []string{"User"}
	}

	tokenString, err := common.SignClaims(claims, app.cfg.JWTSecretKey)
	if err != nil {
		log.Error("[oidc] failed to sign JWT: ", err)
		c.Redirect(http.StatusFound, "/login?error=jwt_failed")
		return
	}

	// Set the same auth cookie as local login
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(cookieName, tokenString, int(expiresAfter.Seconds()), "/", "", app.cfg.HTTPSCookie, true)

	log.Info("[oidc] login successful, user: ", user.ID)

	// Redirect to returnTo or /
	returnTo, _ := c.Cookie(oidcRedirectCookie)
	if returnTo == "" {
		returnTo = "/"
	}
	c.SetCookie(oidcRedirectCookie, "", -1, "/", "", secure, true)
	c.Redirect(http.StatusFound, returnTo)
}

// sanitizeEmail sanitizes an email to match model.sanitizeEmail (same regex).
// We replicate it here because model.sanitizeEmail is unexported.
func sanitizeEmail(email string) string {
	// same regex as model package
	return model.SanitizeEmail(email)
}