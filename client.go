package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const apiTimeout = 5 * time.Second

// ErrAuth means the token is missing or the host API rejected it. Commands
// must fail closed: print the auth notice and exit 1 without doing anything.
var ErrAuth = errors.New("microvms access not configured or revoked")

// UnreachableError reports that the host API could not be reached at all
// (connection refused, DNS failure, broken response).
type UnreachableError struct{ Host string }

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("host unreachable: is %s online?", e.Host)
}

// TimeoutError means the request exceeded the client timeout. The host may
// still have executed the operation: a DELETE, for instance, can be accepted
// and then outlive the timeout while the VM tears down.
type TimeoutError struct {
	Method string
	Path   string
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("request timed out after %s (%s %s); the host may still have completed it - verify with 'list' or just re-run", apiTimeout, e.Method, e.Path)
}

// APIError is any other non-2xx response from the host API.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("host API error %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("host API error %d", e.StatusCode)
}

// transportError classifies a client-side transport failure: a timeout gets
// its own error (the operation may still have completed server-side), any
// other failure means the host could not be reached.
func transportError(err error, method, path, host string) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return &TimeoutError{Method: method, Path: path}
	}
	return &UnreachableError{Host: host}
}

// Client is a minimal JSON client for the microVM host API. No retries: a
// failed call surfaces immediately.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: apiTimeout},
	}
}

// host returns the host (without port) of the base URL, for error messages.
func (c *Client) host() string {
	if u, err := url.Parse(c.BaseURL); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return c.BaseURL
}

func (c *Client) do(method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return transportError(err, method, path, c.host())
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return transportError(err, method, path, c.host())
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrAuth
	}
	if resp.StatusCode >= 400 {
		msg := strings.TrimSpace(string(data))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return &APIError{StatusCode: resp.StatusCode, Message: msg}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode response from %s %s: %w", method, path, err)
		}
	}
	return nil
}

// VM mirrors the host API's VM object. SSHPrivateKey is only returned by the
// create endpoint when the server generated the keypair; it must never be
// printed (see vmOutput in main.go).
type VM struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	State         string `json:"state"`
	IP            string `json:"ip"`
	SSHPort       int    `json:"ssh_port"`
	SSHUser       string `json:"ssh_user"`
	SSHCommand    string `json:"ssh_command"`
	SSHPrivateKey string `json:"ssh_private_key,omitempty"`
	VCPUs         int    `json:"vcpus"`
	MemMiB        int    `json:"mem_mib"`
	CreatedAt     string `json:"created_at"`
}

type vmsList struct {
	VMs []VM `json:"vms"`
}

// Capacity mirrors the host API's capacity object.
type Capacity struct {
	MaxVMs         int `json:"max_vms"`
	ExistingVMs    int `json:"existing_vms"`
	RunningVMs     int `json:"running_vms"`
	MaxVCPUsPerVM  int `json:"max_vcpus_per_vm"`
	MaxMemMiBPerVM int `json:"max_mem_mib_per_vm"`
	FreeHostMemMiB int `json:"free_host_mem_mib"`
	FreeHostCPUs   int `json:"free_host_cpus"`
}

// CreateVMRequest is the body of POST /v1/vms. Empty fields are omitted so
// the host applies its own defaults (ssh_key omitted => server-generated
// keypair, returned as ssh_private_key).
type CreateVMRequest struct {
	Name   string `json:"name,omitempty"`
	CPUs   int    `json:"cpus,omitempty"`
	MemMiB int    `json:"mem_mib,omitempty"`
	SSHKey string `json:"ssh_key,omitempty"`
}

func (c *Client) CreateVM(req CreateVMRequest) (VM, error) {
	var vm VM
	err := c.do(http.MethodPost, "/v1/vms", req, &vm)
	return vm, err
}

func (c *Client) ListVMs() ([]VM, error) {
	var l vmsList
	if err := c.do(http.MethodGet, "/v1/vms", nil, &l); err != nil {
		return nil, err
	}
	return l.VMs, nil
}

func (c *Client) StopVM(id string) (VM, error) {
	var vm VM
	err := c.do(http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/stop", nil, &vm)
	return vm, err
}

func (c *Client) StartVM(id string) (VM, error) {
	var vm VM
	err := c.do(http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/start", nil, &vm)
	return vm, err
}

func (c *Client) DeleteVM(id string) error {
	return c.do(http.MethodDelete, "/v1/vms/"+url.PathEscape(id), nil, nil)
}

func (c *Client) Capacity() (Capacity, error) {
	var cap Capacity
	err := c.do(http.MethodGet, "/v1/capacity", nil, &cap)
	return cap, err
}
