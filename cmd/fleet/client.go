package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Machine struct {
	ID        string     `json:"id"`
	Image     string     `json:"image"`
	Status    string     `json:"status"`
	CreatedAt time.Time  `json:"createdAt"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	ExitedAt  *time.Time `json:"exitedAt,omitempty"`
	Error     string     `json:"error,omitempty"`
}

type listResponse struct {
	Machines []Machine `json:"machines"`
}

type apiClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func newClient(baseURL, apiKey string) *apiClient {
	return &apiClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *apiClient) do(ctx context.Context, method, path string, body any, out any) error {
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return err
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(respBody)))
	}

	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *apiClient) Create(ctx context.Context, image string) (Machine, error) {
	var m Machine
	err := c.do(ctx, http.MethodPost, "/machines",
		map[string]string{"image": image}, &m)
	return m, err
}

func (c *apiClient) List(ctx context.Context) ([]Machine, error) {
	var out listResponse
	if err := c.do(ctx, http.MethodGet, "/machines", nil, &out); err != nil {
		return nil, err
	}
	return out.Machines, nil
}

func (c *apiClient) Get(ctx context.Context, id string) (Machine, error) {
	var m Machine
	err := c.do(ctx, http.MethodGet, "/machines/"+id, nil, &m)
	return m, err
}

func (c *apiClient) Delete(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/machines/"+id, nil, nil)
}
