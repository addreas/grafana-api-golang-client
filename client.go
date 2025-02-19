package gapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"time"

	"github.com/hashicorp/go-cleanhttp"
)

// Client is a Grafana API client.
type Client struct {
	config  Config
	baseURL url.URL
	client  *http.Client
}

// Config contains client configuration.
type Config struct {
	// APIKey is an optional API key or service account token.
	APIKey string
	// BasicAuth is optional basic auth credentials.
	BasicAuth *url.Userinfo
	// HTTPHeaders are optional HTTP headers.
	HTTPHeaders map[string]string
	// Client provides an optional HTTP client, otherwise a default will be used.
	Client *http.Client
	// OrgID provides an optional organization ID
	// with BasicAuth, it defaults to last used org
	// with APIKey, it is disallowed because service account tokens are scoped to a single org
	OrgID int64
	// NumRetries contains the number of attempted retries
	NumRetries int
}

// New creates a new Grafana client.
func New(baseURL string, cfg Config) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}

	if cfg.BasicAuth != nil {
		u.User = cfg.BasicAuth
	}

	cli := cfg.Client
	if cli == nil {
		cli = cleanhttp.DefaultClient()
	}

	return &Client{
		config:  cfg,
		baseURL: *u,
		client:  cli,
	}, nil
}

// WithOrgID returns a new client with the provided organization ID.
func (c Client) WithOrgID(orgID int64) *Client {
	c.config.OrgID = orgID
	return &c
}

type APIError struct {
	StatusCode int
	Body       map[string]interface{}
}

func (e APIError) Error() string {
	return fmt.Sprintf("status: %d, body: %v", e.StatusCode, e.Body)
}

func Request[ReqT any, ResT any](c *Client, method, requestPath string, query url.Values, requestBody *ReqT) (*ResT, error) {
	var err error
	var requestBytes, responseBytes []byte

	if requestBody != nil {
		requestBytes, err = json.Marshal(requestBody)
		if err != nil {
			return nil, err
		}
	}

	var resp *http.Response
	// retry logic
	for n := 0; n <= c.config.NumRetries; n++ {
		req, err := c.newRequest(method, requestPath, query, bytes.NewReader(requestBytes))
		if err != nil {
			return nil, err
		}

		// Wait a bit if that's not the first request
		if n != 0 {
			time.Sleep(time.Second * 5)
		}

		resp, err = c.client.Do(req)

		// If err is not nil, retry again
		// That's either caused by client policy, or failure to speak HTTP (such as network connectivity problem). A
		// non-2xx status code doesn't cause an error.
		if err != nil {
			continue
		}

		defer resp.Body.Close()

		// read the body (even on non-successful HTTP status codes), as that's what the unit tests expect
		responseBytes, err = ioutil.ReadAll(resp.Body)
		// if there was an error reading the body, try again
		if err != nil {
			continue
		}

		// Exit the loop if we have something final to return. This is anything < 500, if it's not a 429.
		if resp.StatusCode < http.StatusInternalServerError && resp.StatusCode != http.StatusTooManyRequests {
			break
		}
	}
	if err != nil {
		return nil, err
	}

	// check status code.
	if resp.StatusCode >= 400 {
		var errContent map[string]interface{}
		err := json.Unmarshal(responseBytes, &errContent)
		if err != nil {
			return nil, fmt.Errorf("failed to json unmarshal error response '%s' for error code %d: %w", responseBytes, resp.StatusCode, err)
		}
		return nil, APIError{
			StatusCode: resp.StatusCode,
			Body:       errContent,
		}
	}

	var responseStruct ResT
	err = json.Unmarshal(responseBytes, &responseStruct)
	if err != nil {
		return nil, err
	}

	return &responseStruct, nil
}

func (c *Client) request(method, requestPath string, query url.Values, body io.Reader, responseStruct interface{}) error {
	var (
		req          *http.Request
		resp         *http.Response
		err          error
		bodyContents []byte
	)

	// If we want to retry a request that sends data, we'll need to stash the request data in memory. Otherwise, we lose it since readers cannot be replayed.
	var bodyBuffer bytes.Buffer
	if c.config.NumRetries > 0 && body != nil {
		body = io.TeeReader(body, &bodyBuffer)
	}

	// retry logic
	for n := 0; n <= c.config.NumRetries; n++ {
		// If it's not the first request, re-use the request body we stashed earlier.
		if n > 0 {
			body = bytes.NewReader(bodyBuffer.Bytes())
		}

		req, err = c.newRequest(method, requestPath, query, body)
		if err != nil {
			return err
		}

		// Wait a bit if that's not the first request
		if n != 0 {
			time.Sleep(time.Second * 5)
		}

		resp, err = c.client.Do(req)

		// If err is not nil, retry again
		// That's either caused by client policy, or failure to speak HTTP (such as network connectivity problem). A
		// non-2xx status code doesn't cause an error.
		if err != nil {
			continue
		}

		defer resp.Body.Close()

		// read the body (even on non-successful HTTP status codes), as that's what the unit tests expect
		bodyContents, err = ioutil.ReadAll(resp.Body)

		// if there was an error reading the body, try again
		if err != nil {
			continue
		}

		// Exit the loop if we have something final to return. This is anything < 500, if it's not a 429.
		if resp.StatusCode < http.StatusInternalServerError && resp.StatusCode != http.StatusTooManyRequests {
			break
		}
	}
	if err != nil {
		return err
	}

	if os.Getenv("GF_LOG") != "" {
		log.Printf("response status %d with body %v", resp.StatusCode, string(bodyContents))
	}

	// check status code.
	if resp.StatusCode >= 400 {
		return fmt.Errorf("status: %d, body: %v", resp.StatusCode, string(bodyContents))
	}

	if responseStruct == nil {
		return nil
	}

	err = json.Unmarshal(bodyContents, responseStruct)
	if err != nil {
		return err
	}

	return nil
}

func (c *Client) newRequest(method, requestPath string, query url.Values, body io.Reader) (*http.Request, error) {
	url := c.baseURL
	url.Path = path.Join(url.Path, requestPath)
	url.RawQuery = query.Encode()
	req, err := http.NewRequest(method, url.String(), body)
	if err != nil {
		return req, err
	}

	// cannot use both API key and org ID. API keys are scoped to single org
	if c.config.APIKey != "" {
		req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", c.config.APIKey))
	}
	if c.config.OrgID != 0 {
		req.Header.Add("X-Grafana-Org-Id", strconv.FormatInt(c.config.OrgID, 10))
	}

	if c.config.HTTPHeaders != nil {
		for k, v := range c.config.HTTPHeaders {
			req.Header.Add(k, v)
		}
	}

	if os.Getenv("GF_LOG") != "" {
		if body == nil {
			log.Printf("request (%s) to %s with no body data", method, url.String())
		} else {
			log.Printf("request (%s) to %s with body data: %s", method, url.String(), body.(*bytes.Buffer).String())
		}
	}

	req.Header.Add("Content-Type", "application/json")
	return req, err
}
