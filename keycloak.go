package keycloak

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/go-querystring/query"
)

const (
	defaultAdminBase = "admin/realms"
	defaultBase      = "realms"

	formEncoded   = "application/x-www-form-urlencoded"
	passwordGrant = "password"
	clientGrant   = "client_credentials"
	offlineScope  = "offline_access"
)

// Response is the Keycloak response.
type Response struct {
	Response *http.Response
}

// ErrorResponse returns the error response from Keycloak
type ErrorResponse struct {
	Response *http.Response
	Message  string `json:"error_description"`
}

func (r *ErrorResponse) Error() string {
	return fmt.Sprintf("%v %v: %d %v",
		r.Response.Request.Method, r.Response.Request.URL,
		r.Response.StatusCode, r.Message)
}

// Client manages communication to Keycloak
type Client struct {
	common     service      // Reuse struct
	httpClient *http.Client // HTTP client to communicate with keycloak

	// Keycloak Client Configuration
	baseURL *url.URL
	realm   string

	hasOfflineAccess bool
	isServiceAccount bool
	isConfidential   bool

	clientID     string
	clientSecret string

	adminAccount string
	adminPass    string

	// Services
	Authentication *AuthenticationService
	AdminUser      *AdminUserService
	UMA            *UMAService

	adminOIDC *OIDCToken
}

type service struct {
	client *Client
}

type headers struct {
	authorization string
	contentType   string
}

// NewServiceAccount is targeted at Service Accounts with elevated privileges
func NewServiceAccount(
	httpClient *http.Client,

	baseURL string,
	realm string,
	hasOfflineAccess bool,

	clientID string,
	clientSecret string,
) *Client {
	return newClient(httpClient, baseURL, realm, hasOfflineAccess, true, true, clientID, clientSecret, "", "")
}

// NewConfidentialAdmin is targeted at users with elevated privileges
// who will be using a confidential client to authenticate against.
func NewConfidentialAdmin(
	httpClient *http.Client,

	baseURL string,
	realm string,
	hasOfflineAccess bool,

	clientID string,
	clientSecret string,

	adminAccount string,
	adminPass string,
) *Client {
	return newClient(httpClient, baseURL, realm, hasOfflineAccess, false, true, clientID, clientSecret, adminAccount, adminPass)
}

// NewPublicAdmin is targeted at users with elevated privileges who will
// be using a public client to authenticate against.
func NewPublicAdmin(
	httpClient *http.Client,

	baseURL string,
	realm string,
	hasOfflineAccess bool,

	clientID string,

	adminAccount string,
	adminPass string,
) *Client {
	return newClient(httpClient, baseURL, realm, hasOfflineAccess, false, false, clientID, "", adminAccount, adminPass)
}

// newClient returns a new Keycloak consumer. If no httpClient is provided
// the default httpClient will be used.
func newClient(
	httpClient *http.Client,

	baseURL string,
	realm string,

	// Requires offline_access role
	hasOfflineAccess bool,
	// Requires confidential access type and service accounts enabled
	isServiceAccount bool,
	// Requires client secret when making protected requests
	isConfidential bool,

	// If using service accounts
	clientID string,
	clientSecret string,

	// If using an admin account
	adminAccount string,
	adminPass string,
) *Client {

	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	base, _ := url.Parse(baseURL)

	c := &Client{
		httpClient: httpClient,
		baseURL:    base,
		realm:      realm,

		hasOfflineAccess: hasOfflineAccess,
		isServiceAccount: isServiceAccount,
		isConfidential:   isConfidential,

		clientID:     clientID,
		clientSecret: clientSecret,

		adminAccount: adminAccount,
		adminPass:    adminPass,
		adminOIDC:    &OIDCToken{},
	}

	c.common.client = c
	c.Authentication = (*AuthenticationService)(&c.common)
	c.AdminUser = (*AdminUserService)(&c.common)
	c.UMA = (*UMAService)(&c.common)

	return c
}

// BaseURL returns the baseURL value
func (c Client) BaseURL() string { return c.baseURL.String() }

// Realm returns the realm value
func (c Client) Realm() string { return c.realm }

// ClientID returns the clientID value
func (c Client) ClientID() string { return c.clientID }

// ClientSecret returns the clientSecret value
func (c Client) ClientSecret() string { return c.clientSecret }

// AdminAccount returns the adminAccount value
func (c Client) AdminAccount() string { return c.adminAccount }

// AdminPass returns the adminPass value
func (c Client) AdminPass() string { return c.adminPass }

// AdminOIDC returns the admin access token
func (c Client) AdminOIDC() *OIDCToken { return c.adminOIDC }

// newRequest creates the keycloak request with a relative URL provided.
func (c *Client) newRequest(
	method,
	path string,
	body interface{},
	h headers,
	isAdminRequest bool,
) (*http.Request, error) {
	u, err := c.baseURL.Parse(path)
	if err != nil {
		return nil, err
	}

	var req *http.Request
	if h.contentType == formEncoded && body != nil {
		formEnc, err := query.Values(body)
		if err != nil {
			return nil, err
		}
		form := strings.NewReader(formEnc.Encode())
		req, err = http.NewRequest(method, u.String(), form)
	} else if body != nil {
		buf := new(bytes.Buffer)
		enc := json.NewEncoder(buf)
		enc.SetEscapeHTML(false)
		err := enc.Encode(body)
		if err != nil {
			return nil, err
		}

		req, err = http.NewRequest(method, u.String(), buf)
	} else {
		req, err = http.NewRequest(method, u.String(), nil)
	}
	if err != nil {
		return nil, err
	}

	if h.contentType != "" {
		req.Header.Set("Content-Type", h.contentType)
	}
	if body != nil && h.contentType == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if h.authorization != "" {
		req.Header.Set("Authorization", h.authorization)
	}
	if isAdminRequest {
		var token *OIDCToken
		var err error

		adminGrant := &AccessGrantRequest{}

		if c.hasOfflineAccess {
			adminGrant.Scope = offlineScope
		}

		if c.isConfidential && c.isServiceAccount {
			adminGrant.GrantType = clientGrant

			token, _, err = c.Authentication.GetOIDCToken(
				context.Background(),
				adminGrant,
			)
		} else {
			adminGrant.GrantType = passwordGrant
			adminGrant.Username = c.adminAccount
			adminGrant.Password = c.adminPass

			token, _, err = c.Authentication.GetOIDCToken(
				context.Background(),
				adminGrant,
			)
		}

		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	}

	return req, nil
}

// do sends a keycloak request and returns the repsonse.
func (c *Client) do(
	ctx context.Context,
	req *http.Request,
	v interface{},
) (*Response, error) {
	req = req.WithContext(ctx)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
	}
	defer resp.Body.Close()

	response := &Response{Response: resp}

	if c := resp.StatusCode; c >= 300 {
		errorResponse := &ErrorResponse{Response: resp}

		data, err := ioutil.ReadAll(resp.Body)
		if err == nil && data != nil {
			json.Unmarshal(data, errorResponse)
		}

		return nil, errorResponse
	}

	if v != nil {
		if w, ok := v.(io.Writer); ok {
			io.Copy(w, resp.Body)
		} else {
			decErr := json.NewDecoder(resp.Body).Decode(v)
			if decErr == io.EOF {
				decErr = nil // ignore empty response errors
			}
			if decErr != nil {
				err = decErr
			}
		}
	}

	return response, err
}
