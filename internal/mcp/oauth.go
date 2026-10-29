// This file implements MCP authorization for HTTP servers: the OAuth 2.1 flow the
// MCP specification layers on top of HTTP — protected-resource metadata discovery,
// authorization-server metadata discovery, dynamic client registration, the
// authorization-code grant with PKCE, and token persistence under
// $PIGO_HOME/mcp-oauth/ so later launches reuse the granted token and refresh it
// instead of prompting again.
//
// Enable it per server with `oauth = true` (or `mcp add ... --oauth`). A server that
// does not require authorization works exactly as before: the flow only runs when
// the endpoint answers 401.
package mcp

import (
	"context"
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
	"sync"
	"time"
)

// oauthTimeout bounds one interactive authorization (the user has to approve in a
// browser).
const oauthTimeout = 5 * time.Minute

// openBrowserFunc opens an authorization URL. It is a variable so tests can follow
// the URL in-process instead of launching a browser.
var openBrowserFunc = launchBrowser

// launchBrowser best-effort opens url with the platform opener; the URL is printed
// by the caller beforehand, so a failure is not fatal.
func launchBrowser(u string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd = "rundll32"
		args = []string{"url.dll,FileProtocolHandler"}
	default:
		cmd = "xdg-open"
	}
	args = append(args, u)
	c := exec.Command(cmd, args...)
	c.Stdout = io.Discard
	c.Stderr = io.Discard
	return c.Start()
}

// oauthProvider owns one server's OAuth state and tokens.
type oauthProvider struct {
	server      string
	resourceURL string
	storePath   string
	http        *http.Client
	logw        io.Writer
	openBrowser func(string) error

	mu            sync.Mutex
	clientID      string
	tokenEndpoint string
	accessToken   string
	refreshToken  string
	expiry        time.Time
}

// storedToken is the on-disk token record.
type storedToken struct {
	ClientID      string    `json:"client_id,omitempty"`
	TokenEndpoint string    `json:"token_endpoint,omitempty"`
	AccessToken   string    `json:"access_token,omitempty"`
	RefreshToken  string    `json:"refresh_token,omitempty"`
	Expiry        time.Time `json:"expiry,omitempty"`
	Resource      string    `json:"resource,omitempty"`
}

// newOAuthProvider builds the provider for cfg and loads any cached token.
func newOAuthProvider(cfg ServerConfig, logw io.Writer) *oauthProvider {
	p := &oauthProvider{
		server:      cfg.Name,
		resourceURL: strings.TrimSpace(cfg.URL),
		http:        &http.Client{Timeout: 30 * time.Second},
		logw:        logw,
		openBrowser: openBrowserFunc,
	}
	if dir := oauthStoreDir(); dir != "" {
		p.storePath = filepath.Join(dir, sanitizeToolName(cfg.Name)+".json")
	}
	p.load()
	return p
}

// oauthStoreDir is $PIGO_HOME/mcp-oauth (default ~/.pigo/mcp-oauth).
func oauthStoreDir() string {
	base := os.Getenv("PIGO_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".pigo")
	}
	return filepath.Join(base, "mcp-oauth")
}

// load reads the cached token, ignoring a missing or unreadable file.
func (p *oauthProvider) load() {
	if p.storePath == "" {
		return
	}
	data, err := os.ReadFile(p.storePath)
	if err != nil {
		return
	}
	var st storedToken
	if err := json.Unmarshal(data, &st); err != nil {
		return
	}
	p.clientID = st.ClientID
	p.tokenEndpoint = st.TokenEndpoint
	p.accessToken = st.AccessToken
	p.refreshToken = st.RefreshToken
	p.expiry = st.Expiry
}

// save writes the token record (0600) best-effort.
func (p *oauthProvider) save() {
	if p.storePath == "" {
		return
	}
	st := storedToken{
		ClientID:      p.clientID,
		TokenEndpoint: p.tokenEndpoint,
		AccessToken:   p.accessToken,
		RefreshToken:  p.refreshToken,
		Expiry:        p.expiry,
		Resource:      p.resourceURL,
	}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p.storePath), 0o700); err != nil {
		return
	}
	_ = os.WriteFile(p.storePath, data, 0o600)
}

// Authorization returns the Authorization header value to send, or "" when no valid
// token is available yet (the caller then hits 401 and runs authorize).
func (p *oauthProvider) Authorization(ctx context.Context) (string, error) {
	p.mu.Lock()
	token := p.accessToken
	expired := !p.expiry.IsZero() && time.Now().After(p.expiry.Add(-30*time.Second))
	refresh := p.refreshToken
	p.mu.Unlock()
	if token != "" && !expired {
		return "Bearer " + token, nil
	}
	if refresh != "" {
		if err := p.refresh(ctx); err == nil {
			p.mu.Lock()
			token = p.accessToken
			p.mu.Unlock()
			if token != "" {
				return "Bearer " + token, nil
			}
		}
	}
	return "", nil
}

// Invalidate drops the access token (a 401 means it was rejected) while keeping the
// refresh token.
func (p *oauthProvider) Invalidate() {
	p.mu.Lock()
	p.accessToken = ""
	p.expiry = time.Time{}
	p.mu.Unlock()
	p.save()
}

// resourceMetadata is the protected-resource metadata document.
type resourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

// authServerMetadata is the authorization-server metadata document.
type authServerMetadata struct {
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	ScopesSupported       []string `json:"scopes_supported"`
}

// authorize runs the full interactive flow and stores the resulting token.
func (p *oauthProvider) authorize(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, oauthTimeout)
	defer cancel()

	as, err := p.discover(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	clientID := p.clientID
	p.mu.Unlock()

	// Loopback redirect: the authorization code is delivered here.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("oauth: listen for the callback: %w", err)
	}
	defer ln.Close()
	redirectURI := "http://" + ln.Addr().String() + "/callback"

	if clientID == "" {
		clientID, err = p.register(ctx, as.RegistrationEndpoint, redirectURI)
		if err != nil {
			return err
		}
	}

	verifier, challenge, err := pkcePair()
	if err != nil {
		return err
	}
	state, err := randomToken()
	if err != nil {
		return err
	}

	codeCh := make(chan string, 1)
	mux := http.NewServeMux()
	var once sync.Once
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		if e := q.Get("error"); e != "" {
			http.Error(w, "authorization failed: "+e, http.StatusBadRequest)
			return
		}
		fmt.Fprintln(w, "pigo is authorized. You can close this window.")
		once.Do(func() { codeCh <- q.Get("code") })
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	authURL, err := url.Parse(as.AuthorizationEndpoint)
	if err != nil {
		return fmt.Errorf("oauth: authorization endpoint: %w", err)
	}
	q := authURL.Query()
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	q.Set("resource", p.resourceURL)
	if scope := strings.Join(as.ScopesSupported, " "); scope != "" {
		q.Set("scope", scope)
	}
	authURL.RawQuery = q.Encode()

	if p.logw != nil {
		fmt.Fprintf(p.logw, "pigo: mcp server %q needs authorization; open this URL to approve:\n  %s\n", p.server, authURL.String())
	}
	if p.openBrowser != nil {
		_ = p.openBrowser(authURL.String())
	}

	var code string
	select {
	case code = <-codeCh:
	case <-ctx.Done():
		return fmt.Errorf("oauth: timed out waiting for the authorization code: %w", ctx.Err())
	}
	if code == "" {
		return fmt.Errorf("oauth: no authorization code received")
	}

	tok, err := p.exchange(ctx, as.TokenEndpoint, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
		"resource":      {p.resourceURL},
	})
	if err != nil {
		return err
	}
	p.apply(clientID, as.TokenEndpoint, tok)
	return nil
}

// discover resolves the authorization-server metadata for the resource.
func (p *oauthProvider) discover(ctx context.Context) (authServerMetadata, error) {
	var as authServerMetadata
	base, err := url.Parse(p.resourceURL)
	if err != nil {
		return as, fmt.Errorf("oauth: parse url: %w", err)
	}
	origin := base.Scheme + "://" + base.Host

	// Protected-resource metadata (RFC 9728): try the path-aware form first, then
	// the origin-level document.
	var rm resourceMetadata
	found := false
	for _, candidate := range []string{
		origin + "/.well-known/oauth-protected-resource" + strings.TrimSuffix(base.Path, "/"),
		origin + "/.well-known/oauth-protected-resource",
	} {
		if err := p.getJSON(ctx, candidate, &rm); err == nil {
			found = true
			break
		}
	}
	asBase := origin
	if found && len(rm.AuthorizationServers) > 0 {
		asBase = strings.TrimSuffix(rm.AuthorizationServers[0], "/")
	}

	for _, candidate := range []string{
		asBase + "/.well-known/oauth-authorization-server",
		asBase + "/.well-known/openid-configuration",
	} {
		if err := p.getJSON(ctx, candidate, &as); err == nil && as.AuthorizationEndpoint != "" && as.TokenEndpoint != "" {
			if len(as.ScopesSupported) == 0 {
				as.ScopesSupported = rm.ScopesSupported
			}
			return as, nil
		}
	}
	return as, fmt.Errorf("oauth: no authorization server metadata at %s", asBase)
}

// getJSON fetches and decodes a metadata document.
func (p *oauthProvider) getJSON(ctx context.Context, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", rawURL, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// register performs dynamic client registration (RFC 7591) and returns the client id.
func (p *oauthProvider) register(ctx context.Context, endpoint, redirectURI string) (string, error) {
	if endpoint == "" {
		return "", fmt.Errorf("oauth: the authorization server offers no registration endpoint and no client_id is configured")
	}
	body, err := json.Marshal(map[string]any{
		"client_name":                "pigo",
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"application_type":           "native",
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("oauth: register client: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("oauth: register client: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var reg struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&reg); err != nil {
		return "", fmt.Errorf("oauth: decode registration: %w", err)
	}
	if reg.ClientID == "" {
		return "", fmt.Errorf("oauth: registration returned no client_id")
	}
	return reg.ClientID, nil
}

// tokenResponse is the token endpoint reply.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// exchange posts a token request.
func (p *oauthProvider) exchange(ctx context.Context, endpoint string, form url.Values) (tokenResponse, error) {
	var tok tokenResponse
	if endpoint == "" {
		return tok, fmt.Errorf("oauth: no token endpoint")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tok, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return tok, fmt.Errorf("oauth: token request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return tok, fmt.Errorf("oauth: token request: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return tok, fmt.Errorf("oauth: decode token: %w", err)
	}
	if tok.AccessToken == "" {
		return tok, fmt.Errorf("oauth: token response carried no access_token")
	}
	return tok, nil
}

// refresh uses the stored refresh token to obtain a fresh access token.
func (p *oauthProvider) refresh(ctx context.Context) error {
	p.mu.Lock()
	refresh := p.refreshToken
	clientID := p.clientID
	endpoint := p.tokenEndpoint
	p.mu.Unlock()
	if refresh == "" || endpoint == "" {
		return fmt.Errorf("oauth: no refresh token")
	}
	tok, err := p.exchange(ctx, endpoint, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {clientID},
		"resource":      {p.resourceURL},
	})
	if err != nil {
		return err
	}
	p.apply(clientID, endpoint, tok)
	return nil
}

// apply stores a granted token (keeping the refresh token when the server omits it).
func (p *oauthProvider) apply(clientID, endpoint string, tok tokenResponse) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clientID = clientID
	p.tokenEndpoint = endpoint
	p.accessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		p.refreshToken = tok.RefreshToken
	}
	if tok.ExpiresIn > 0 {
		p.expiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	} else {
		p.expiry = time.Time{}
	}
	p.save()
}

// pkcePair returns a verifier and its S256 challenge.
func pkcePair() (string, string, error) {
	verifier, err := randomToken()
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// randomToken returns 32 bytes of URL-safe randomness.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
