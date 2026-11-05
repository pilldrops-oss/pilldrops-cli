package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// APIError is a non-2xx portal response, carrying the server's error message
// and its own error code.
type APIError struct {
	Status int
	Msg    string
	// Code is the platform's stable code — slug_taken, egress_not_enforced,
	// confirm_required, backups_need_object_store — passed through verbatim so
	// a caller can branch on it. Empty when the refusal carried none; a
	// transport failure is not an APIError at all, so a caller that cannot see
	// a terminal can still tell the two apart.
	Code string
	// Body is the response as sent, for the routes whose refusal says more than
	// {error, code}.
	Body []byte
}

func (e *APIError) Error() string { return fmt.Sprintf("%s (HTTP %d)", e.Msg, e.Status) }

// Client is a thin typed wrapper over the portal API.
type Client struct {
	Base string // e.g. https://portal.example — no trailing slash
	Key  string
	HTTP *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// Raw performs a request and returns the response body. GETs are retried once
// on transport errors or 5xx (idempotent); writes are never retried.
func (c *Client) Raw(method, path string, body []byte, contentType string) ([]byte, error) {
	attempt := func() ([]byte, int, error) {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, c.Base+path, rd)
		if err != nil {
			return nil, 0, err
		}
		if c.Key != "" {
			req.Header.Set("Authorization", "Bearer "+c.Key)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := c.http().Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return b, resp.StatusCode, err
	}

	b, status, err := attempt()
	if method == "GET" && (err != nil || status >= 500) {
		time.Sleep(300 * time.Millisecond)
		b, status, err = attempt()
	}
	if err != nil {
		return nil, err
	}
	if status < 200 || status > 299 {
		return nil, apiError(status, b)
	}
	return b, nil
}

func apiError(status int, body []byte) *APIError {
	var e struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	_ = json.Unmarshal(body, &e)
	if e.Error == "" {
		e.Error = http.StatusText(status)
	}
	return &APIError{Status: status, Msg: e.Error, Code: e.Code, Body: body}
}

// Stream opens a server-sent event stream. It has no timeout, unlike Raw: a
// healthy stream never ends. The caller closes the body.
func (c *Client) Stream(path string) (io.ReadCloser, error) {
	req, err := http.NewRequest("GET", c.Base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return nil, apiError(resp.StatusCode, b)
	}
	return resp.Body, nil
}

// JSON performs a request with a JSON body (in may be nil) and decodes the
// JSON response into out (out may be nil).
func (c *Client) JSON(method, path string, in, out any) error {
	var body []byte
	ctype := ""
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
		ctype = "application/json"
	}
	b, err := c.Raw(method, path, body, ctype)
	if err != nil {
		return err
	}
	if out == nil || len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, out)
}
