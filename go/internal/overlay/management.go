// Package overlay is the control plane's side of the OpenZiti management API:
// minting, listing and deleting the identities of the relays that reach it over
// the overlay, and taking them away again.
//
// The control plane logs in with its own enrolled identity's certificate, so
// no password is configured anywhere for this. That identity is an overlay
// admin, which is broader than minting relays needs; see
// docs/OPENZITI-AND-P2P.md.
package overlay

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// IdentityFile is the part of an enrolled OpenZiti identity this package reads.
type IdentityFile struct {
	// ZtAPI is the controller's client API, which is how the identity says
	// which network it belongs to.
	ZtAPI string `json:"ztAPI"`
	ID    struct {
		Cert string `json:"cert"`
		Key  string `json:"key"`
		CA   string `json:"ca"`
	} `json:"id"`
}

// ReadIdentityFile parses an enrolled identity. Only inline PEM ("pem:...") is
// supported, which is what `ziti edge enroll` writes.
func ReadIdentityFile(path string) (IdentityFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return IdentityFile{}, err
	}
	var file IdentityFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return IdentityFile{}, fmt.Errorf("parsing the identity file: %w", err)
	}
	for _, field := range []*string{&file.ID.Cert, &file.ID.Key, &file.ID.CA} {
		if !strings.HasPrefix(*field, "pem:") {
			return IdentityFile{}, errors.New("the identity file must hold its certificate, key and CA inline (pem:)")
		}
		*field = strings.TrimPrefix(*field, "pem:")
	}
	return file, nil
}

// Config says how to reach the management API.
type Config struct {
	// URL is the management API's base, e.g.
	// https://10.20.0.22:1281/edge/management/v1.
	URL string
	// ServerName is what the controller's certificate is checked against when
	// URL's host is not in it: the management API is reached on an internal
	// address and the certificate names the public one.
	ServerName string
	// Identity is the enrolled identity to log in as.
	Identity IdentityFile
	// HTTP overrides the transport, for tests. Its TLS settings are replaced.
	HTTP *http.Client
}

// Client calls the management API.
type Client struct {
	base string
	http *http.Client

	mu      sync.Mutex
	session string
}

// NewClient builds a client; it does not connect until the first call.
func NewClient(config Config) (*Client, error) {
	if config.URL == "" {
		return nil, errors.New("the management API needs a URL")
	}
	certificate, err := tls.X509KeyPair([]byte(config.Identity.ID.Cert), []byte(config.Identity.ID.Key))
	if err != nil {
		return nil, fmt.Errorf("the identity's certificate: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(config.Identity.ID.CA)) {
		return nil, errors.New("the identity's CA bundle holds no certificates")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{
		Certificates: []tls.Certificate{certificate},
		RootCAs:      roots,
		ServerName:   config.ServerName,
		MinVersion:   tls.VersionTLS12,
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	if config.HTTP != nil {
		client = config.HTTP
	}
	return &Client{base: strings.TrimRight(config.URL, "/"), http: client}, nil
}

// Identity is an overlay identity as the control plane sees it.
type Identity struct {
	ID             string
	Name           string
	RoleAttributes []string
	Tags           map[string]string
	// Enrolled is whether the one-time token has been spent.
	Enrolled bool
}

// NewIdentity is what Create makes.
type NewIdentity struct {
	Name           string
	RoleAttributes []string
	Tags           map[string]string
	// TokenExpires, when set, replaces the controller's default lifetime for
	// the one-time enrolment token.
	TokenExpires time.Time
}

// Enrolment is a created identity and the one-time token that enrols it.
type Enrolment struct {
	ID        string
	Name      string
	JWT       string
	ExpiresAt time.Time
}

// ErrNotFound is an identity that does not exist.
var ErrNotFound = errors.New("no such overlay identity")

// apiError is the management API refusing a call.
type apiError struct {
	Status int
	Code   string
	Detail string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("management API: %d %s %s", e.Status, e.Code, e.Detail)
}

type envelope struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) login(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != "" {
		return c.session, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/authenticate?method=cert",
		strings.NewReader("{}"))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return "", fmt.Errorf("logging in to the management API: %w", err)
	}
	defer response.Body.Close()
	var body struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if response.StatusCode != http.StatusOK {
		return "", &apiError{Status: response.StatusCode, Code: "login", Detail: "certificate login refused"}
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body); err != nil || body.Data.Token == "" {
		return "", errors.New("the management API returned no session")
	}
	c.session = body.Data.Token
	return c.session, nil
}

func (c *Client) forget(session string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == session {
		c.session = ""
	}
}

// call makes one request, logging in first and once more if the session has
// lapsed. out may be nil.
func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return err
		}
	}
	for attempt := 0; ; attempt++ {
		session, err := c.login(ctx)
		if err != nil {
			return err
		}
		request, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("zt-session", session)
		response, err := c.http.Do(request)
		if err != nil {
			return err
		}
		raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
		response.Body.Close()
		if err != nil {
			return err
		}
		if response.StatusCode == http.StatusUnauthorized && attempt == 0 {
			c.forget(session)
			continue
		}
		var parsed envelope
		_ = json.Unmarshal(raw, &parsed)
		if response.StatusCode == http.StatusNotFound {
			return ErrNotFound
		}
		if response.StatusCode/100 != 2 {
			failure := &apiError{Status: response.StatusCode}
			if parsed.Error != nil {
				failure.Code, failure.Detail = parsed.Error.Code, parsed.Error.Message
			}
			return failure
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(parsed.Data, out)
	}
}

type identityDetail struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	RoleAttributes []string          `json:"roleAttributes"`
	Tags           map[string]any    `json:"tags"`
	Authenticators map[string]any    `json:"authenticators"`
	Enrollment     map[string]ottRef `json:"enrollment"`
}

type ottRef struct {
	ID        string    `json:"id"`
	JWT       string    `json:"jwt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (d identityDetail) identity() Identity {
	tags := map[string]string{}
	for key, value := range d.Tags {
		if text, ok := value.(string); ok {
			tags[key] = text
		}
	}
	_, enrolled := d.Authenticators["cert"]
	return Identity{ID: d.ID, Name: d.Name, RoleAttributes: d.RoleAttributes, Tags: tags, Enrolled: enrolled}
}

// Find returns the identity with this name, or ErrNotFound.
func (c *Client) Find(ctx context.Context, name string) (Identity, error) {
	found, err := c.list(ctx, fmt.Sprintf("name=%q", name), 1)
	if err != nil {
		return Identity{}, err
	}
	if len(found) == 0 {
		return Identity{}, ErrNotFound
	}
	return found[0], nil
}

// WithRole lists every identity carrying a role attribute.
func (c *Client) WithRole(ctx context.Context, role string) ([]Identity, error) {
	return c.list(ctx, fmt.Sprintf("anyOf(roleAttributes)=%q", role), 0)
}

func (c *Client) list(ctx context.Context, filter string, max int) ([]Identity, error) {
	const page = 500
	var all []Identity
	for offset := 0; ; offset += page {
		query := url.Values{"filter": {filter}, "limit": {fmt.Sprint(page)}, "offset": {fmt.Sprint(offset)}}
		var details []identityDetail
		if err := c.call(ctx, http.MethodGet, "/identities?"+query.Encode(), nil, &details); err != nil {
			return nil, err
		}
		for _, detail := range details {
			all = append(all, detail.identity())
			if max > 0 && len(all) == max {
				return all, nil
			}
		}
		if len(details) < page {
			return all, nil
		}
	}
}

// Create makes an identity with a one-time enrolment token and returns both.
func (c *Client) Create(ctx context.Context, asked NewIdentity) (Enrolment, error) {
	if asked.Name == "" {
		return Enrolment{}, errors.New("an identity needs a name")
	}
	roles := asked.RoleAttributes
	if roles == nil {
		roles = []string{}
	}
	tags := map[string]string{}
	for key, value := range asked.Tags {
		tags[key] = value
	}
	var created struct {
		ID string `json:"id"`
	}
	err := c.call(ctx, http.MethodPost, "/identities", map[string]any{
		"name":           asked.Name,
		"type":           "Default",
		"isAdmin":        false,
		"roleAttributes": roles,
		"tags":           tags,
		"enrollment":     map[string]bool{"ott": true},
	}, &created)
	if err != nil {
		return Enrolment{}, err
	}
	if !asked.TokenExpires.IsZero() {
		if err := c.reissue(ctx, created.ID, asked.TokenExpires); err != nil {
			_ = c.Delete(context.WithoutCancel(ctx), created.ID)
			return Enrolment{}, err
		}
	}
	var detail identityDetail
	if err := c.call(ctx, http.MethodGet, "/identities/"+created.ID, nil, &detail); err != nil {
		return Enrolment{}, err
	}
	ott, ok := detail.Enrollment["ott"]
	if !ok || ott.JWT == "" {
		return Enrolment{}, errors.New("the controller returned no enrolment token")
	}
	return Enrolment{ID: created.ID, Name: asked.Name, JWT: ott.JWT, ExpiresAt: ott.ExpiresAt}, nil
}

// reissue replaces an identity's enrolment token with one expiring at until.
func (c *Client) reissue(ctx context.Context, id string, until time.Time) error {
	var detail identityDetail
	if err := c.call(ctx, http.MethodGet, "/identities/"+id, nil, &detail); err != nil {
		return err
	}
	if ott, ok := detail.Enrollment["ott"]; ok && ott.ID != "" {
		if err := c.call(ctx, http.MethodDelete, "/enrollments/"+ott.ID, nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	return c.call(ctx, http.MethodPost, "/enrollments", map[string]any{
		"method":     "ott",
		"identityId": id,
		"expiresAt":  until.UTC().Format(time.RFC3339),
	}, nil)
}

// Delete removes an identity, which also ends its sessions and circuits. An
// identity that is already gone is not an error.
func (c *Client) Delete(ctx context.Context, id string) error {
	err := c.call(ctx, http.MethodDelete, "/identities/"+id, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}
