package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// -------- OAuth 2.1 for remote MCP servers --------
//
// Flow: discover protected-resource metadata (RFC 9728), then the
// authorization server metadata, register a client dynamically (RFC 7591)
// because most MCP servers have no pre-registered client, run
// authorization-code + PKCE against a loopback redirect, and store the
// tokens for later calls. Refresh happens transparently on expiry or 401.

type MCPAuthCfg struct {
	Type           string   `json:"type"` // "oauth" | "bearer"
	ClientID       string   `json:"client_id,omitempty"`
	ClientSecret   string   `json:"client_secret,omitempty"`
	Scopes         []string `json:"scopes,omitempty"`
	RedirectPort   int      `json:"redirect_port,omitempty"`
	TokenCachePath string   `json:"token_cache_path,omitempty"`
}

type protectedResourceMeta struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

type authServerMeta struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	ScopesSupported       []string `json:"scopes_supported"`
}

type oauthTokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

type mcpTokenFile struct {
	Server        string  `json:"server"`
	Resource      string  `json:"resource"`
	TokenEndpoint string  `json:"token_endpoint"`
	ClientID      string  `json:"client_id"`
	ClientSecret  string  `json:"client_secret,omitempty"`
	Scope         string  `json:"scope,omitempty"`
	AccessToken   string  `json:"access_token"`
	RefreshToken  string  `json:"refresh_token,omitempty"`
	ExpiresAt     float64 `json:"expires_at"`
}

var oauthHTTP = &http.Client{Timeout: 30 * time.Second}

// -------- token store --------

func mcpTokenPath(name string, cfg MCPServerCfg) string {
	if cfg.Auth != nil && cfg.Auth.TokenCachePath != "" {
		return cfg.Auth.TokenCachePath
	}
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		return ""
	}
	return filepath.Join(dir, "ronin", "mcp-"+sanitizeBase(name)+".json")
}

func loadMCPToken(p string) (mcpTokenFile, bool) {
	var t mcpTokenFile
	if p == "" {
		return t, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return t, false
	}
	if json.Unmarshal(b, &t) != nil || t.AccessToken == "" {
		return t, false
	}
	return t, true
}

func saveMCPToken(p string, t mcpTokenFile) error {
	if p == "" {
		return fmt.Errorf("no token path available")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// -------- discovery --------

func fetchJSONInto(u string, out any) error {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := oauthHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

// discoverProtectedResource finds RFC 9728 metadata for the MCP endpoint.
// Well-known locations differ by spec revision, so several are tried.
func discoverProtectedResource(mcpURL string) (protectedResourceMeta, error) {
	var meta protectedResourceMeta
	u, err := url.Parse(mcpURL)
	if err != nil {
		return meta, err
	}
	path := strings.TrimSuffix(u.Path, "/")
	candidates := []string{
		u.Scheme + "://" + u.Host + "/.well-known/oauth-protected-resource" + path,
		u.Scheme + "://" + u.Host + "/.well-known/oauth-protected-resource",
		u.Scheme + "://" + u.Host + path + "/.well-known/oauth-protected-resource",
	}
	var lastErr error
	for _, c := range candidates {
		if err := fetchJSONInto(c, &meta); err == nil && len(meta.AuthorizationServers) > 0 {
			return meta, nil
		} else if err != nil {
			lastErr = err
		}
	}
	return meta, fmt.Errorf("protected resource metadata not found: %v", lastErr)
}

func discoverAuthServer(issuer string) (authServerMeta, error) {
	var meta authServerMeta
	base := strings.TrimSuffix(issuer, "/")
	for _, suffix := range []string{
		"/.well-known/oauth-authorization-server",
		"/.well-known/openid-configuration",
	} {
		if err := fetchJSONInto(base+suffix, &meta); err == nil && meta.TokenEndpoint != "" && meta.AuthorizationEndpoint != "" {
			return meta, nil
		}
	}
	return meta, fmt.Errorf("authorization server metadata not found for %s", issuer)
}

// -------- PKCE + registration --------

func pkcePair() (verifier, challenge string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomState() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// registerClient performs dynamic client registration (RFC 7591).
func registerClient(as authServerMeta, redirectURI string, scopes []string) (clientID, clientSecret string, err error) {
	if as.RegistrationEndpoint == "" {
		return "", "", fmt.Errorf("authorization server offers no registration_endpoint; set auth.client_id manually")
	}
	body := map[string]any{
		"client_name":                "ronin-agent",
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	}
	if len(scopes) > 0 {
		body["scope"] = strings.Join(scopes, " ")
	}
	raw, _ := json.Marshal(body)
	resp, err := oauthHTTP.Post(as.RegistrationEndpoint, "application/json", strings.NewReader(string(raw)))
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return "", "", fmt.Errorf("registration failed: http %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", "", err
	}
	if out.ClientID == "" {
		return "", "", fmt.Errorf("registration returned no client_id")
	}
	return out.ClientID, out.ClientSecret, nil
}

// -------- token exchange --------

func tokenRequest(endpoint string, form url.Values) (oauthTokenResp, error) {
	var tok oauthTokenResp
	resp, err := oauthHTTP.PostForm(endpoint, form)
	if err != nil {
		return tok, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(b, &tok); err != nil {
		return tok, fmt.Errorf("token endpoint returned non-json: %s", strings.TrimSpace(string(b)))
	}
	if tok.Error != "" {
		return tok, fmt.Errorf("token endpoint: %s %s", tok.Error, tok.ErrorDesc)
	}
	if resp.StatusCode/100 != 2 || tok.AccessToken == "" {
		return tok, fmt.Errorf("token endpoint: http %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return tok, nil
}

func exchangeCode(meta authServerMeta, clientID, clientSecret, code, redirectURI, verifier string) (oauthTokenResp, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	}
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}
	return tokenRequest(meta.TokenEndpoint, form)
}

func refreshMCPToken(p string, t mcpTokenFile) (mcpTokenFile, error) {
	if t.RefreshToken == "" {
		return t, fmt.Errorf("no refresh token")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {t.RefreshToken},
		"client_id":     {t.ClientID},
	}
	if t.ClientSecret != "" {
		form.Set("client_secret", t.ClientSecret)
	}
	tok, err := tokenRequest(t.TokenEndpoint, form)
	if err != nil {
		return t, err
	}
	t.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		t.RefreshToken = tok.RefreshToken
	}
	if tok.ExpiresIn > 0 {
		t.ExpiresAt = float64(time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix())
	} else {
		t.ExpiresAt = float64(time.Now().Add(50 * time.Minute).Unix())
	}
	if err := saveMCPToken(p, t); err != nil {
		return t, err
	}
	return t, nil
}

// ensureMCPToken returns a usable access token, refreshing if needed. It
// never starts an interactive flow: that is what "ronin mcp login" is for.
func ensureMCPToken(name string, cfg MCPServerCfg) (string, error) {
	if cfg.Auth == nil || cfg.Auth.Type != "oauth" {
		return "", nil
	}
	p := mcpTokenPath(name, cfg)
	t, ok := loadMCPToken(p)
	if !ok {
		if p == "" {
			return "", fmt.Errorf("cannot determine a cache directory; run: ronin mcp login %s", name)
		}
		if _, statErr := os.Stat(p); statErr != nil {
			return "", fmt.Errorf("no token file at %s (%v); run: ronin mcp login %s", p, statErr, name)
		}
		return "", fmt.Errorf("token file at %s is unreadable or empty; run: ronin mcp login %s", p, name)
	}
	if t.ExpiresAt == 0 || float64(time.Now().Unix()) < t.ExpiresAt-60 {
		return t.AccessToken, nil
	}
	t, err := refreshMCPToken(p, t)
	if err != nil {
		return "", fmt.Errorf("refresh failed (%v); run: ronin mcp login %s", err, name)
	}
	return t.AccessToken, nil
}

func openBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}

func listenLoopback(port int) (net.Listener, string, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, "", err
	}
	addr := ln.Addr().(*net.TCPAddr)
	return ln, fmt.Sprintf("http://127.0.0.1:%d/callback", addr.Port), nil
}

// -------- interactive login --------

func mcpLogin(name string, cfg MCPServerCfg) int {
	if cfg.URL == "" {
		fmt.Fprintln(os.Stderr, "login requires an MCP server configured with \"url\"")
		return 2
	}
	if cfg.Auth == nil {
		cfg.Auth = &MCPAuthCfg{Type: "oauth"}
	}
	if cfg.Auth.Type != "oauth" {
		fmt.Fprintln(os.Stderr, "server auth.type is not oauth")
		return 2
	}

	fmt.Fprintf(os.Stderr, "discovering auth for %s\n", cfg.URL)
	prm, err := discoverProtectedResource(cfg.URL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "authorization server: %s\n", prm.AuthorizationServers[0])

	as, err := discoverAuthServer(prm.AuthorizationServers[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	scopes := cfg.Auth.Scopes
	if len(scopes) == 0 {
		scopes = prm.ScopesSupported
	}

	ln, redirectURI, err := listenLoopback(cfg.Auth.RedirectPort)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		return 1
	}
	defer ln.Close()

	clientID := cfg.Auth.ClientID
	clientSecret := cfg.Auth.ClientSecret
	if clientID == "" {
		fmt.Fprintln(os.Stderr, "registering client")
		clientID, clientSecret, err = registerClient(as, redirectURI, scopes)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "client_id: %s\n", clientID)
	}

	verifier, challenge := pkcePair()
	state := randomState()

	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	authURL := as.AuthorizationEndpoint + "?" + q.Encode()

	fmt.Fprint(os.Stderr, "\nopen this URL to authorize:\n\n")
	fmt.Fprintln(os.Stderr, authURL)
	fmt.Fprintln(os.Stderr)
	if err := openBrowser(authURL); err != nil {
		fmt.Fprintf(os.Stderr, "(could not open a browser automatically: %v)\n", err)
	}

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("state"); got != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- fmt.Errorf("state mismatch")
			return
		}
		if e := r.URL.Query().Get("error"); e != "" {
			fmt.Fprintf(w, "authorization failed: %s", e)
			errCh <- fmt.Errorf("authorization error: %s", e)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "no code", http.StatusBadRequest)
			errCh <- fmt.Errorf("no authorization code in callback")
			return
		}
		fmt.Fprintln(w, "Ronin authorized. You can close this tab.")
		codeCh <- code
	})}
	go srv.Serve(ln)
	defer srv.Close()

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		fmt.Fprintln(os.Stderr, err)
		return 1
	case <-time.After(5 * time.Minute):
		fmt.Fprintln(os.Stderr, "timed out waiting for authorization")
		return 1
	}

	tok, err := exchangeCode(as, clientID, clientSecret, code, redirectURI, verifier)
	if err != nil {
		fmt.Fprintln(os.Stderr, "token exchange:", err)
		return 1
	}

	tf := mcpTokenFile{
		Server:        name,
		Resource:      prm.Resource,
		TokenEndpoint: as.TokenEndpoint,
		ClientID:      clientID,
		ClientSecret:  clientSecret,
		Scope:         tok.Scope,
		AccessToken:   tok.AccessToken,
		RefreshToken:  tok.RefreshToken,
	}
	if tok.ExpiresIn > 0 {
		tf.ExpiresAt = float64(time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix())
	}
	p := mcpTokenPath(name, cfg)
	if err := saveMCPToken(p, tf); err != nil {
		fmt.Fprintln(os.Stderr, "save token:", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "\nlogged in; token stored at %s\n", p)
	return 0
}

// forceRefreshMCPToken refreshes even when the stored expiry says the
// token is still valid. Used after a 401, where the server has revoked
// access that the local clock knows nothing about.
func forceRefreshMCPToken(name string, cfg MCPServerCfg) (string, error) {
	p := mcpTokenPath(name, cfg)
	t, ok := loadMCPToken(p)
	if !ok {
		return "", fmt.Errorf("no stored token for %q; run: ronin mcp login %s", name, name)
	}
	t, err := refreshMCPToken(p, t)
	if err != nil {
		return "", fmt.Errorf("refresh failed (%v); run: ronin mcp login %s", err, name)
	}
	return t.AccessToken, nil
}
