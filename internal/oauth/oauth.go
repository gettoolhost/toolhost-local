// Package oauth is the driven adapter for upstream OAuth: it builds the
// go-sdk OAuthHandlers (client-credentials via extauth, auth-code+PKCE via
// auth.AuthorizationCodeHandler) and owns the on-disk token store that lets
// `serve` run unattended after `toolhost auth` granted once.
package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/auth/extauth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"toolhost/internal/config"
)

// DefaultRedirectURL is the loopback callback the authorization flow listens
// on. A fixed port keeps preregistered and DCR'd redirect URIs stable —
// CLIs do this everywhere (gh, az, gcloud all do).
const DefaultRedirectURL = "http://127.0.0.1:8484/callback"

// StorePath is where upstream tokens live: beside the config file, never in
// it — the config is shareable, the tokens are credentials.
func StorePath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "toolhost_tokens.json")
}

// StoredToken is one backend's persisted grant, including enough of the
// client registration (id/secret/token endpoint) to rebuild a refreshing
// token source on the next run without re-authorizing.
type StoredToken struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
	Scopes       []string  `json:"scopes,omitempty"`
	TokenURL     string    `json:"token_url,omitempty"`
	ClientID     string    `json:"client_id,omitempty"`
	ClientSecret string    `json:"client_secret,omitempty"`
}

type storeFile struct {
	Backends map[string]*StoredToken `json:"backends"`
}

// Store persists upstream tokens in <config dir>/toolhost_tokens.json (0600).
type Store struct {
	path     string
	mu       sync.Mutex
	backends map[string]*StoredToken
}

func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, backends: map[string]*StoredToken{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("read token store %s: %w", path, err)
	}
	var f storeFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse token store %s: %w", path, err)
	}
	for k, v := range f.Backends {
		s.backends[k] = v
	}
	return s, nil
}

func (s *Store) Path() string { return s.path }

// Get returns the stored token for a backend, or nil.
func (s *Store) Get(name string) *StoredToken {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.backends[name]
}

// Put records or replaces a backend's token and flushes the file.
func (s *Store) Put(name string, tok *StoredToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.backends[name] = tok
	return s.flushLocked()
}

// Delete removes a backend's token (revoke/re-auth).
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.backends, name)
	return s.flushLocked()
}

func (s *Store) flushLocked() error {
	raw, err := json.MarshalIndent(storeFile{Backends: s.backends}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, append(raw, '\n'), 0o600)
}

// Source returns a token source for a stored grant, refreshing and
// re-persisting as needed. Nil when nothing is stored — callers treat nil
// as "needs `toolhost auth`".
func (s *Store) Source(ctx context.Context, name string) oauth2.TokenSource {
	st := s.Get(name)
	if st == nil {
		return nil
	}
	return s.wrap(ctx, name, st)
}

// wrap rebuilds the refreshing source from a stored grant and persists any
// token the refresh produces.
func (s *Store) wrap(ctx context.Context, name string, st *StoredToken) oauth2.TokenSource {
	cfg := &oauth2.Config{
		ClientID:     st.ClientID,
		ClientSecret: st.ClientSecret,
		Scopes:       st.Scopes,
		Endpoint:     oauth2.Endpoint{TokenURL: st.TokenURL},
	}
	base := cfg.TokenSource(ctx, &oauth2.Token{
		AccessToken:  st.AccessToken,
		TokenType:    st.TokenType,
		RefreshToken: st.RefreshToken,
		Expiry:       st.Expiry,
	})
	return &persistSource{
		base: base,
		seen: st.AccessToken,
		save: func(t *oauth2.Token) {
			merged := *st
			merged.AccessToken = t.AccessToken
			merged.TokenType = t.TokenType
			merged.Expiry = t.Expiry
			if t.RefreshToken != "" {
				merged.RefreshToken = t.RefreshToken
			}
			_ = s.Put(name, &merged)
		},
	}
}

// persistSource writes every freshly issued token back to the store — the
// refresh path is otherwise invisible and a rotated refresh token would be
// lost between runs.
type persistSource struct {
	base oauth2.TokenSource
	mu   sync.Mutex
	seen string
	save func(*oauth2.Token)
}

func (p *persistSource) Token() (*oauth2.Token, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t, err := p.base.Token()
	if err != nil {
		return nil, err
	}
	if t.AccessToken != p.seen {
		p.seen = t.AccessToken
		p.save(t)
	}
	return t, nil
}

// NewHandler builds the go-sdk OAuthHandler for a backend's auth block —
// nil for auth modes that don't need one (none/bearer get their header from
// the base client instead). interactive=false installs a fetcher that fails
// fast ("run toolhost auth") so serve/discover never pop a browser. open is
// the browser launcher — nil picks the platform default.
func NewHandler(ctx context.Context, name string, a *config.Auth, store *Store, interactive bool, open func(string) error, out io.Writer) (auth.OAuthHandler, error) {
	if a == nil {
		return nil, nil
	}
	switch a.Type {
	case "", "none", "bearer":
		// no handler — bearer is a static header on the base client.
		return nil, nil

	case "client_credentials":
		return extauth.NewClientCredentialsHandler(&extauth.ClientCredentialsHandlerConfig{
			Credentials: &oauthex.ClientCredentials{
				ClientID:         a.ClientID,
				ClientSecretAuth: &oauthex.ClientSecretAuth{ClientSecret: a.ClientSecret},
			},
		})

	case "oauth":
		redirect := a.RedirectURL
		if redirect == "" {
			redirect = DefaultRedirectURL
		}
		fetcher := failFetcher(name)
		if interactive {
			if open == nil {
				open = defaultOpen
			}
			fetcher = LoopbackFetcher(redirect, open, out)
		}
		cfg := &auth.AuthorizationCodeHandlerConfig{
			RedirectURL:              redirect,
			AuthorizationCodeFetcher: fetcher,
			RequestRefreshToken:      true,
		}
		// A nil store means "this run can't persist" — the flow still works,
		// the token just dies with the process (discover's posture).
		if store != nil {
			cfg.InitialTokenSource = store.Source(ctx, name)
			cfg.NewTokenSource = store.newPersistingSource(name)
		}
		if a.ClientID != "" {
			creds := &oauthex.ClientCredentials{ClientID: a.ClientID}
			if a.ClientSecret != "" {
				creds.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: a.ClientSecret}
			}
			cfg.PreregisteredClient = creds
		} else {
			cfg.DynamicClientRegistrationConfig = &auth.DynamicClientRegistrationConfig{
				Metadata: &oauthex.ClientRegistrationMetadata{
					RedirectURIs:            []string{redirect},
					ClientName:              "toolhost",
					GrantTypes:              []string{"authorization_code", "refresh_token"},
					ResponseTypes:           []string{"code"},
					TokenEndpointAuthMethod: "none",
				},
			}
		}
		return auth.NewAuthorizationCodeHandler(cfg)
	}
	return nil, fmt.Errorf("no oauth handler for auth.type %q", a.Type)
}

// newPersistingSource is the NewTokenSource hook: the SDK calls it with the
// freshly-exchanged token AND the oauth2.Config it came from — exactly the
// pieces needed to rebuild the source (and keep persisting refreshes) later.
func (s *Store) newPersistingSource(name string) func(context.Context, *oauth2.Config, *oauth2.Token) (oauth2.TokenSource, error) {
	return func(ctx context.Context, cfg *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
		st := &StoredToken{
			AccessToken:  tok.AccessToken,
			TokenType:    tok.TokenType,
			RefreshToken: tok.RefreshToken,
			Expiry:       tok.Expiry,
			Scopes:       cfg.Scopes,
			TokenURL:     cfg.Endpoint.TokenURL,
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
		}
		if err := s.Put(name, st); err != nil {
			return nil, fmt.Errorf("persist token for backend %q: %w", name, err)
		}
		return s.wrap(ctx, name, st), nil
	}
}

// failFetcher is installed when interactive=false: any authorization attempt
// fails closed with the remedy instead of silently hanging a serve.
func failFetcher(name string) auth.AuthorizationCodeFetcher {
	return func(context.Context, *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		return nil, fmt.Errorf("backend %q needs an OAuth grant — run: toolhost auth %s", name, name)
	}
}

// AuthorizeOnce drives a handler against a real 401 — the same probe the
// streamable transport performs internally on connect, invoked explicitly so
// `toolhost auth` (and SSE, which has no OAuthHandler seam) can run the flow.
func AuthorizeOnce(ctx context.Context, endpoint string, h auth.OAuthHandler, hc *http.Client) error {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"toolhost","version":"0"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("probe %s: %w", endpoint, err)
	}
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		resp.Body.Close()
		return fmt.Errorf("endpoint answered %d without an auth challenge", resp.StatusCode)
	}
	// h.Authorize consumes resp (the WWW-Authenticate challenge drives
	// discovery); it must not be closed beforehand.
	return h.Authorize(ctx, req, resp)
}
