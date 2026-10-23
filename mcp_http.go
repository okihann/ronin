package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func newHTTPMCPClient(name string, cfg MCPServerCfg) *mcpClient {
	return &mcpClient{
		name:    name,
		cfg:     cfg,
		url:     cfg.URL,
		headers: cfg.Headers,
		httpc:   &http.Client{Timeout: 5 * time.Minute},
	}
}

// httpPost sends one JSON-RPC message, carrying the session id once the
// server has assigned one.
func (c *mcpClient) httpPost(payload []byte) (*http.Response, error) {
	return c.doPost(payload, false)
}

// doPost sends one JSON-RPC message. When force is set the access token is
// refreshed first, which is how a 401 gets retried.
func (c *mcpClient) doPost(payload []byte, force bool) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	var tok string
	if force {
		tok, _ = forceRefreshMCPToken(c.name, c.cfg)
	} else {
		tok, _ = ensureMCPToken(c.name, c.cfg)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sessionID = sid
	}
	return resp, nil
}

func (c *mcpClient) httpRequest(method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.id++
	id := c.id
	c.mu.Unlock()

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	resp, err := c.httpPost(body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && c.cfg.Auth != nil && c.cfg.Auth.Type == "oauth" {
		resp.Body.Close()
		if resp, err = c.doPost(body, true); err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("mcp http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return readSSE(resp.Body, id)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return nil, err
	}
	var r rpcResp
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("mcp: bad json response: %w", err)
	}
	if r.Error != nil {
		return nil, fmt.Errorf("mcp error %d: %s", r.Error.Code, r.Error.Message)
	}
	return r.Result, nil
}

// readSSE scans an event stream until the response carrying id arrives,
// skipping server-initiated notifications interleaved on the same stream.
func readSSE(r io.Reader, id int64) (json.RawMessage, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if line != "" {
			if strings.HasPrefix(line, "data:") {
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
			continue
		}
		if data.Len() == 0 {
			continue
		}
		raw := data.String()
		data.Reset()
		var r rpcResp
		if json.Unmarshal([]byte(raw), &r) != nil {
			continue
		}
		if r.ID != id {
			continue
		}
		if r.Error != nil {
			return nil, fmt.Errorf("mcp error %d: %s", r.Error.Code, r.Error.Message)
		}
		return r.Result, nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("mcp: stream closed before a response arrived")
}

func (c *mcpClient) httpNotify(method string, params any) error {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": method, "params": params,
	})
	resp, err := c.httpPost(body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	return nil
}

// httpClose ends the session politely; the spec asks clients to DELETE.
func (c *mcpClient) httpClose() {
	if c.sessionID == "" || c.url == "" {
		return
	}
	req, err := http.NewRequest(http.MethodDelete, c.url, nil)
	if err != nil {
		return
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Mcp-Session-Id", c.sessionID)
	if resp, err := c.httpc.Do(req); err == nil {
		resp.Body.Close()
	}
}
