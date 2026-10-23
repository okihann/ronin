package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// -------- MCP config --------

// MCPServerCfg describes one MCP server Ronin can act as a client to.
type MCPServerCfg struct {
	Command       string            `json:"command"`
	Args          []string          `json:"args"`
	Env           map[string]string `json:"env"`
	URL           string            `json:"url"`
	Auth          *MCPAuthCfg       `json:"auth,omitempty"`
	Headers       map[string]string `json:"headers"`
	DefaultPolicy string            `json:"default_policy"` // allow | confirm | deny
	Tools         map[string]string `json:"tools"`
}

type MCPCfg struct {
	Servers map[string]MCPServerCfg `json:"servers"`
}

// policyFor resolves the effective policy for a tool. Unlisted tools
// fall back to default_policy, which defaults to deny (fail-closed).
func (s MCPServerCfg) policyFor(tool string) string {
	if p, ok := s.Tools[tool]; ok && p != "" {
		return p
	}
	if s.DefaultPolicy != "" {
		return s.DefaultPolicy
	}
	return "deny"
}

// -------- JSON-RPC --------

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcErr         `json:"error"`
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type mcpClient struct {
	cmd   *exec.Cmd
	stdin interface {
		Write([]byte) (int, error)
		Close() error
	}
	stdout    *bufio.Reader
	url       string
	headers   map[string]string
	sessionID string
	name      string
	cfg       MCPServerCfg
	httpc     *http.Client
	mu        sync.Mutex
	id        int64
}

func startMCP(name string, cfg MCPServerCfg) (*mcpClient, error) {
	if cfg.URL != "" && cfg.Command != "" {
		return nil, fmt.Errorf("mcp server: set either command or url, not both")
	}
	var c *mcpClient
	if cfg.URL != "" {
		c = newHTTPMCPClient(name, cfg)
	} else {
		if cfg.Command == "" {
			return nil, fmt.Errorf("mcp server: command or url is required")
		}
		cmd := exec.Command(cfg.Command, cfg.Args...)
		env := os.Environ()
		for k, v := range cfg.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		c = &mcpClient{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}
	}
	if _, err := c.request("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "ronin", "version": "1.0.0"},
	}); err != nil {
		c.close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	c.notify("notifications/initialized", map[string]any{})
	return c, nil
}

func (c *mcpClient) request(method string, params any) (json.RawMessage, error) {
	if c.url != "" {
		return c.httpRequest(method, params)
	}
	c.mu.Lock()
	c.id++
	id := c.id
	c.mu.Unlock()
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	b, _ := json.Marshal(req)
	if _, err := c.stdin.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	for {
		line, err := c.stdout.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r rpcResp
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		if r.ID != id {
			continue // notification or unrelated response
		}
		if r.Error != nil {
			return nil, fmt.Errorf("mcp error %d: %s", r.Error.Code, r.Error.Message)
		}
		return r.Result, nil
	}
}

func (c *mcpClient) notify(method string, params any) {
	if c.url != "" {
		_ = c.httpNotify(method, params)
		return
	}
	msg := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	b, _ := json.Marshal(msg)
	_, _ = c.stdin.Write(append(b, '\n'))
}

func (c *mcpClient) listTools() ([]mcpTool, error) {
	var all []mcpTool
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := c.request("tools/list", params)
		if err != nil {
			return nil, err
		}
		var out struct {
			Tools      []mcpTool `json:"tools"`
			NextCursor string    `json:"nextCursor"`
		}
		if err := json.Unmarshal(res, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Tools...)
		if out.NextCursor == "" {
			break
		}
		cursor = out.NextCursor
	}
	return all, nil
}

func (c *mcpClient) callTool(name string, args map[string]any) (json.RawMessage, error) {
	return c.request("tools/call", map[string]any{"name": name, "arguments": args})
}

func (c *mcpClient) close() {
	if c.url != "" {
		c.httpClose()
		return
	}
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	done := make(chan struct{})
	go func() {
		_ = c.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
	}
}

// -------- helpers --------

func argsHash(args map[string]any) string {
	// encoding/json sorts map keys, so this is deterministic.
	b, _ := json.Marshal(args)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func sortedServerNames(m map[string]MCPServerCfg) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func printToolResult(res json.RawMessage) bool {
	var out struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Data     string `json:"data"`
			MimeType string `json:"mimeType"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err == nil && len(out.Content) > 0 {
		for _, c := range out.Content {
			switch c.Type {
			case "image":
				path, err := saveImageBlock(c.Data, c.MimeType)
				if err != nil {
					fmt.Printf("[image %s, %d b64 chars; save failed: %v]\n", c.MimeType, len(c.Data), err)
				} else {
					fmt.Printf("[image saved: %s (%s)]\n", path, c.MimeType)
				}
			case "resource":
				fmt.Println("[resource content omitted]")
			default:
				if c.Text != "" {
					fmt.Println(c.Text)
				}
			}
		}
		return out.IsError
	}
	fmt.Println(string(res))
	return false
}

// saveImageBlock decodes an MCP image content block and writes it to
// disk, returning the path. Text-only clients (bash, a chat box) cannot
// render pixels, but a file on disk can be inspected or uploaded by hand.
func saveImageBlock(b64, mime string) (string, error) {
	dir := os.Getenv("RONIN_MCP_IMAGE_DIR")
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "ronin-mcp-images")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", err
	}
	ext := ".bin"
	switch mime {
	case "image/png":
		ext = ".png"
	case "image/jpeg":
		ext = ".jpg"
	case "image/webp":
		ext = ".webp"
	case "image/gif":
		ext = ".gif"
	}
	f, err := os.CreateTemp(dir, "mcp-*"+ext)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(raw); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// -------- subcommands --------

func runMCP(args []string) int {
	cfgPath := os.Getenv("RONIN_CONFIG")
	var rest []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-config" && i+1 < len(args):
			cfgPath = args[i+1]
			i++
		case strings.HasPrefix(args[i], "-config="):
			cfgPath = strings.TrimPrefix(args[i], "-config=")
		default:
			rest = append(rest, args[i])
		}
	}
	if cfgPath == "" {
		cfgPath = defaultConfigPath()
	}
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ronin mcp [-config path] list")
		fmt.Fprintln(os.Stderr, "       ronin mcp [-config path] call <server>.<tool> '<json-args>'")
		return 2
	}
	c, err := loadConfig(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		return 1
	}
	switch rest[0] {
	case "list":
		return mcpList(c)
	case "login":
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "usage: ronin mcp login <server>")
			return 2
		}
		srv, ok := c.MCP.Servers[rest[1]]
		if !ok {
			fmt.Fprintln(os.Stderr, "unknown MCP server:", rest[1])
			return 2
		}
		return mcpLogin(rest[1], srv)
	case "call":
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "usage: ronin mcp call <server>.<tool> '<json-args>'")
			return 2
		}
		argJSON := "{}"
		if len(rest) >= 3 {
			argJSON = rest[2]
		}
		return mcpCall(c, rest[1], argJSON)
	default:
		fmt.Fprintln(os.Stderr, "unknown mcp subcommand:", rest[0])
		return 2
	}
}

func mcpList(c Config) int {
	if len(c.MCP.Servers) == 0 {
		fmt.Println("no MCP servers configured (add an mcp.servers block to config.json)")
		return 0
	}
	for _, name := range sortedServerNames(c.MCP.Servers) {
		s := c.MCP.Servers[name]
		endpoint := s.Command
		if s.URL != "" {
			endpoint = s.URL
		}
		fmt.Printf("== %s (%s) ==\n", name, endpoint)
		cl, err := startMCP(name, s)
		if err != nil {
			fmt.Printf("  [failed to start: %v]\n", err)
			continue
		}
		tools, err := cl.listTools()
		cl.close()
		if err != nil {
			fmt.Printf("  [failed to list tools: %v]\n", err)
			continue
		}
		if len(tools) == 0 {
			fmt.Println("  [no tools]")
		}
		for _, t := range tools {
			fmt.Printf("  %-32s [%s]  %s\n", name+"."+t.Name, s.policyFor(t.Name), firstLine(t.Description))
		}
	}
	return 0
}

func mcpCall(c Config, qualified, argJSON string) int {
	server, tool, ok := strings.Cut(qualified, ".")
	if !ok || server == "" || tool == "" {
		fmt.Fprintln(os.Stderr, "expected <server>.<tool>, got:", qualified)
		return 2
	}
	s, ok := c.MCP.Servers[server]
	if !ok {
		fmt.Fprintln(os.Stderr, "unknown MCP server:", server)
		return 2
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argJSON), &args); err != nil {
		fmt.Fprintln(os.Stderr, "bad args json:", err)
		return 2
	}
	if args == nil {
		args = map[string]any{}
	}
	policy := s.policyFor(tool)
	approvalKey := server + "." + tool + "." + argsHash(args)
	switch policy {
	case "deny":
		fmt.Fprintf(os.Stderr, "blocked by MCP policy: %s.%s is denied\n", server, tool)
		return 126
	case "confirm":
		if os.Getenv("RONIN_MCP_APPROVAL") != approvalKey {
			fmt.Fprintf(os.Stderr, "[ronin-mcp-confirm] server=%s tool=%s approval=%s\n", server, tool, approvalKey)
			return 125
		}
	}
	cl, err := startMCP(server, s)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start MCP server:", err)
		return 1
	}
	defer cl.close()
	res, err := cl.callTool(tool, args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "call failed:", err)
		return 1
	}
	if printToolResult(res) {
		return 1
	}
	return 0
}
