// Package account signs tjuclaw in to a TJUClaw account with the device
// flow and talks to the account's library API with the issued token.
//
// The token is stored in the tjuclaw config directory (auth.json, owner-only),
// or taken from TJUCLAW_TOKEN, which is how another agent or a CI job uses
// tjuclaw without an interactive sign-in. It is never printed.
package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultAPI is the API's direct edge address. The web app's own /api passes
// through a page function that gives up after about 15 s, which a large file
// on a slow uplink exceeds; this address has no such limit.
const DefaultAPI = "https://auth.tjuclaw.cloud/api"

// legacyAPI is the address earlier versions stored; it reaches the same API.
const legacyAPI = "https://app.tjuclaw.cloud/api"

// CanonicalAPI maps an older stored address to the one now used.
func CanonicalAPI(api string) string {
	if strings.TrimRight(api, "/") == legacyAPI {
		return DefaultAPI
	}
	return api
}

var (
	ErrSignedOut = errors.New("signed_out")
	ErrDenied    = errors.New("login_denied")
	ErrExpired   = errors.New("login_expired")
)

// RemoteError is the API's stable machine code, never its free text.
type RemoteError struct {
	Status int
	ID     string
}

func (e *RemoteError) Error() string { return e.ID }

// IsCode reports whether err is the API error with this machine code.
func IsCode(err error, id string) bool {
	var remote *RemoteError
	return errors.As(err, &remote) && remote.ID == id
}

type Credentials struct {
	API   string `json:"api"`
	Token string `json:"token"`
	Email string `json:"email,omitempty"`
}

func credentialsPath(dir string) string { return filepath.Join(dir, "auth.json") }

// APIBase is TJUCLAW_API_URL or the production API. Only HTTPS is accepted,
// except a loopback address for local development.
func APIBase() (string, error) {
	base := strings.TrimRight(os.Getenv("TJUCLAW_API_URL"), "/")
	if base == "" {
		return DefaultAPI, nil
	}
	return base, checkBase(base)
}

func checkBase(base string) error {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" {
		return errors.New("invalid_api_url")
	}
	if u.Scheme == "https" {
		return nil
	}
	if ip := net.ParseIP(u.Hostname()); u.Scheme == "http" && (u.Hostname() == "localhost" || ip != nil && ip.IsLoopback()) {
		return nil
	}
	return errors.New("invalid_api_url")
}

// Load returns the credentials in use: TJUCLAW_TOKEN first, then auth.json.
func Load(dir string) (Credentials, error) {
	if token := strings.TrimSpace(os.Getenv("TJUCLAW_TOKEN")); token != "" {
		base, err := APIBase()
		if err != nil {
			return Credentials{}, err
		}
		return Credentials{API: base, Token: token}, nil
	}
	data, err := os.ReadFile(credentialsPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return Credentials{}, ErrSignedOut
	}
	if err != nil {
		return Credentials{}, err
	}
	var c Credentials
	if json.Unmarshal(data, &c) != nil || c.Token == "" || checkBase(c.API) != nil {
		return Credentials{}, ErrSignedOut
	}
	c.API = CanonicalAPI(c.API)
	return c, nil
}

// Save writes auth.json readable by its owner only, replacing it atomically.
func Save(dir string, c Credentials) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".auth-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), credentialsPath(dir))
}

func Remove(dir string) error {
	err := os.Remove(credentialsPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Client calls the account API. With an empty token it can only sign in.
type Client struct {
	API   string
	Token string
	HTTP  *http.Client
}

func NewClient(c Credentials) *Client {
	return &Client{API: c.API, Token: c.Token, HTTP: &http.Client{
		Timeout:       2 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Do sends one request. body may be nil, JSON-able, []byte (sent as is) or
// *Multipart. out, when not nil, receives the JSON response.
func (c *Client) Do(ctx context.Context, method, path string, body any, header map[string]string, out any) error {
	var reader io.Reader
	contentType := ""
	switch b := body.(type) {
	case nil:
	case []byte:
		reader, contentType = bytes.NewReader(b), "application/octet-stream"
	case *Multipart:
		reader, contentType = bytes.NewReader(b.body.Bytes()), b.contentType
	default:
		data, err := json.Marshal(b)
		if err != nil {
			return err
		}
		reader, contentType = bytes.NewReader(data), "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, c.API+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("api_unreachable")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		var envelope struct {
			Error struct {
				ID string `json:"id"`
			} `json:"error"`
		}
		data, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		id := "api_error"
		if json.Unmarshal(data, &envelope) == nil && envelope.Error.ID != "" {
			id = envelope.Error.ID
		}
		return &RemoteError{Status: res.StatusCode, ID: id}
	}
	if raw, ok := out.(*[]byte); ok {
		data, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
		*raw = data
		return err
	}
	if out == nil || res.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(out)
}

// Multipart is a form with one file, as the library upload expects.
type Multipart struct {
	body        bytes.Buffer
	contentType string
}

func NewUpload(fileName, contentType string, data []byte, fields map[string]string) (*Multipart, error) {
	m := &Multipart{}
	w := multipart.NewWriter(&m.body)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return nil, err
		}
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, fileName))
	h.Set("Content-Type", contentType)
	part, err := w.CreatePart(h)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	m.contentType = w.FormDataContentType()
	return m, nil
}

// Device is a started sign-in: show the code and link, then Wait.
type Device struct {
	Code            string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	CompleteURI     string `json:"verification_uri_complete"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
	deviceCode      string
}

func (c *Client) StartLogin(ctx context.Context, name string) (*Device, error) {
	var out struct {
		Device
		DeviceCode string `json:"device_code"`
	}
	if err := c.Do(ctx, "POST", "/cli/device", map[string]string{"name": name}, nil, &out); err != nil {
		return nil, err
	}
	if out.DeviceCode == "" || out.Code == "" {
		return nil, errors.New("api_error")
	}
	d := out.Device
	d.deviceCode = out.DeviceCode
	if d.Interval < 1 {
		d.Interval = 5
	}
	return &d, nil
}

// Wait polls until the code is approved, denied or expires.
func (c *Client) Wait(ctx context.Context, d *Device) (Credentials, error) {
	interval := time.Duration(d.Interval) * time.Second
	deadline := time.Now().Add(time.Duration(max(d.ExpiresIn, 60)) * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return Credentials{}, ctx.Err()
		case <-time.After(interval):
		}
		var out struct {
			Token string `json:"access_token"`
			Email string `json:"email"`
		}
		err := c.Do(ctx, "POST", "/cli/device/token", map[string]string{"device_code": d.deviceCode}, nil, &out)
		switch {
		case err == nil && out.Token != "":
			return Credentials{API: c.API, Token: out.Token, Email: out.Email}, nil
		case IsCode(err, "authorization_pending"):
		case IsCode(err, "slow_down"):
			interval += 5 * time.Second
		case IsCode(err, "access_denied"):
			return Credentials{}, ErrDenied
		case IsCode(err, "expired_token"):
			return Credentials{}, ErrExpired
		case err != nil:
			return Credentials{}, err
		}
	}
	return Credentials{}, ErrExpired
}
