package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"sandbox-cloud/internal/cloud"
	"sandbox-cloud/internal/runner"
)

type Client struct {
	Base string
	HTTP *http.Client
}

func New(base string) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("invalid API URL")
	}
	if u.Scheme == "http" && u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1" {
		return nil, fmt.Errorf("HTTP API URL must be loopback; use kubectl port-forward")
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 20 * time.Second
	return &Client{strings.TrimRight(base, "/"), &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *Client) request(ctx context.Context, method, path string, input io.Reader) (*http.Response, error) {
	r, err := http.NewRequestWithContext(ctx, method, c.Base+path, input)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 16384))
		if err != nil {
			return nil, err
		}
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, e.Error)
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return resp, nil
}
func (c *Client) json(ctx context.Context, method, path string, input, output any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	resp, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if output == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(output)
}
func path(id string) string { return "/v1/workspaces/" + url.PathEscape(id) }
func (c *Client) Create(ctx context.Context, r cloud.CreateRequest) (cloud.Workspace, error) {
	var w cloud.Workspace
	err := c.json(ctx, "POST", "/v1/workspaces", r, &w)
	return w, err
}
func (c *Client) Get(ctx context.Context, id string) (cloud.Workspace, error) {
	var w cloud.Workspace
	err := c.json(ctx, "GET", path(id), nil, &w)
	return w, err
}
func (c *Client) List(ctx context.Context) ([]cloud.Workspace, error) {
	var w []cloud.Workspace
	err := c.json(ctx, "GET", "/v1/workspaces", nil, &w)
	return w, err
}
func (c *Client) Destroy(ctx context.Context, id string) error {
	return c.json(ctx, "DELETE", path(id), nil, nil)
}
func (c *Client) Put(ctx context.Context, id, name string, body io.Reader) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := c.request(ctx, "PUT", path(id)+"/files?path="+url.QueryEscape(name), body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}
func (c *Client) Start(ctx context.Context, id string, r runner.Request) (runner.Result, error) {
	var result runner.Result
	err := c.json(ctx, "POST", path(id)+"/commands", r, &result)
	return result, err
}
func (c *Client) Command(ctx context.Context, id, cmd string) (runner.Result, error) {
	var result runner.Result
	err := c.json(ctx, "GET", path(id)+"/commands/"+url.PathEscape(cmd), nil, &result)
	return result, err
}
func (c *Client) Cancel(ctx context.Context, id, cmd string) error {
	return c.json(ctx, "POST", path(id)+"/commands/"+url.PathEscape(cmd)+"/cancel", nil, nil)
}
func (c *Client) Events(ctx context.Context, id, cmd string, consume func(runner.Event) error) (runner.Result, error) {
	resp, err := c.request(ctx, "GET", path(id)+"/commands/"+url.PathEscape(cmd)+"/events", nil)
	if err != nil {
		return runner.Result{}, err
	}
	defer resp.Body.Close()
	d := json.NewDecoder(resp.Body)
	for {
		var e runner.Event
		if err := d.Decode(&e); err != nil {
			return runner.Result{}, fmt.Errorf("output stream ended without a final result; command may still be running: %w", err)
		}
		if err := consume(e); err != nil {
			return runner.Result{}, err
		}
		if e.Result != nil {
			return *e.Result, nil
		}
	}
}
