package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// -------- config --------

type NgrokCfg struct {
	Enabled   bool   `json:"enabled"`
	Authtoken string `json:"authtoken"`
	Domain    string `json:"domain"`
	DataDir   string `json:"data_dir"`
}

type Config struct {
	Port           int      `json:"port"`
	Root           string   `json:"root"`
	AllowRoots     []string `json:"allow_roots"`
	Timeout        string   `json:"timeout"`
	Token          string   `json:"token"`
	ReadOnly       bool     `json:"read_only"`
	AllowedOrigins []string `json:"allowed_origins"`
	Ngrok          NgrokCfg `json:"ngrok"`
	MCP            MCPCfg   `json:"mcp"`
}

func defaultConfig() Config {
	return Config{
		Port:           8080,
		Root:           ".",
		AllowRoots:     []string{},
		Timeout:        "5m",
		Token:          "",
		ReadOnly:       false,
		AllowedOrigins: []string{"https://chat.deepseek.com", "https://chatgpt.com", "https://chat.openai.com", "https://claude.ai", "https://gemini.google.com", "http://localhost:8080", "http://127.0.0.1:8080"},
		MCP:            MCPCfg{Servers: map[string]MCPServerCfg{}},
		Ngrok: NgrokCfg{
			Enabled:   false,
			Authtoken: "",
			Domain:    "",
			DataDir:   "./.ngrok",
		},
	}
}

func loadConfig(path string) (Config, error) {
	var c Config
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	c = defaultConfig()
	if err := json.Unmarshal(data, &c); err != nil {
		return c, err
	}
	return c, nil
}

func genToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

func writeDefaultConfig(path string) error {
	c := defaultConfig()
	c.Token = genToken() // secure-by-default: never ship an empty token
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}
func defaultConfigPath() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "config.json")
	}
	return "config.json"
}

// -------- runtime state --------

var (
	cfg            Config
	timeout        time.Duration
	defaultRoot    string
	allowRoots     []string
	authToken      string
	readOnly       bool
	configFilePath string
	mcpApproval    string

	ngrokURL       string
	ngrokMu        sync.RWMutex
	ngrokCmd       *exec.Cmd
	allowedOrigins []string
)

// ngrok's local inspection API. Default port is 4040 and is not
// configurable via CLI flag in ngrok v3 -- only via config file.
const ngrokWebAddr = "127.0.0.1:4040"

// -------- security: blocklists & patterns --------

var (
	hardBlocklist   []*regexp.Regexp
	confirmPatterns []*regexp.Regexp
	readOnlyFirsts  map[string]bool
	readOnlyGitSubs map[string]bool
	safeRedir       *regexp.Regexp
)

func init() {
	// ---- hard blocklist: always rejected, no override ----
	hardBlocklist = []*regexp.Regexp{
		// fork bomb
		regexp.MustCompile(`:\(\)\s*\{\s*:\|:&\s*\}\s*;:`),
		// rm -rf / and variants
		regexp.MustCompile(`\brm\s+-[a-zA-Z]*[rR][a-zA-Z]*f[a-zA-Z]*\s+(/|~|\$HOME|/\*)`),
		regexp.MustCompile(`\brm\s+-[a-zA-Z]*f[a-zA-Z]*[rR][a-zA-Z]*\s+(/|~|\$HOME|/\*)`),
		// filesystem creation
		regexp.MustCompile(`\bmkfs\b`),
		// power control
		regexp.MustCompile(`\b(shutdown|reboot|halt|poweroff)\b`),
		regexp.MustCompile(`\binit\s+[06]\b`),
	}

	// ---- confirm list: rejected unless X-Ronin-Confirm: yes ----
	confirmPatterns = []*regexp.Regexp{
		// privilege escalation
		regexp.MustCompile(`\b(sudo|su|doas)\b`),
		// system package managers
		regexp.MustCompile(`\b(apt|apt-get|aptitude|yum|dnf|pacman|zypper|brew|portage|emerge|pkg)\s+(install|remove|purge|update|upgrade|autoremove|dist-upgrade|full-upgrade|reinstall)\b`),
		// language package managers (global installs)
		regexp.MustCompile(`\b(pip|pip3)\s+(install|uninstall)\b`),
		regexp.MustCompile(`\bnpm\s+(install|i|uninstall)\s+(-g|--global)\b`),
		regexp.MustCompile(`\byarn\s+global\b`),
		regexp.MustCompile(`\bpnpm\s+(add|install|remove)\s+(-g|--global)\b`),
		regexp.MustCompile(`\bcargo\s+(install|publish)\b`),
		regexp.MustCompile(`\bgem\s+install\b`),
		regexp.MustCompile(`\bgo\s+install\b`),
		regexp.MustCompile(`\bcomposer\s+global\b`),
		// service control
		regexp.MustCompile(`\bsystemctl\s+(start|stop|restart|reload|enable|disable|mask|unmask|daemon-reload)\b`),
		regexp.MustCompile(`\bservice\s+\S+\s+(start|stop|restart|reload)\b`),
		// containers
		regexp.MustCompile(`\bdocker\s+(run|exec|rm|rmi|stop|kill|prune|system|volume|network)\b`),
		regexp.MustCompile(`\bpodman\s+(run|exec|rm|rmi|stop|kill|prune)\b`),
		regexp.MustCompile(`\bkubectl\s+(apply|delete|create|edit|patch|replace|scale|rollout)\b`),
		// users and groups
		regexp.MustCompile(`\b(useradd|userdel|usermod|groupadd|groupdel|gpasswd|passwd|chsh|chfn)\b`),
		// firewall
		regexp.MustCompile(`\b(iptables|ip6tables|nft|ufw|firewall-cmd)\b`),
		// kernel modules / sysctl
		regexp.MustCompile(`\b(modprobe|insmod|rmmod|sysctl)\b`),
		// mount / partition
		regexp.MustCompile(`\b(mount|umount|fdisk|parted|losetup)\b`),
		// cron / ssh
		regexp.MustCompile(`\bcrontab\b`),
		regexp.MustCompile(`\bssh-keygen\b`),
		// pipe-to-shell
		regexp.MustCompile(`\b(curl|wget)\b[^|]*\|\s*(ba|z|fi)?sh\b`),
		// aggressive process kill
		regexp.MustCompile(`\b(killall|pkill)\s+-9\b`),
		// destructive git
		regexp.MustCompile(`\bgit\s+push\s+(--force|-f)\b`),
		regexp.MustCompile(`\bgit\s+reset\s+--hard\b`),
		regexp.MustCompile(`\bgit\s+clean\s+-[a-zA-Z]*[fF]`),
		regexp.MustCompile(`\bgit\s+filter-branch\b`),
		// package publishing
		regexp.MustCompile(`\bnpm\s+publish\b`),
		// system-directory writes (now confirm, not hard-block; /dev/null unaffected)
		regexp.MustCompile(`\x3e\s*/(etc|boot|sys|proc|root)\b`),
		// recursive permission changes on the filesystem root
		regexp.MustCompile(`\bchmod\s+-R\s+777\s+\x2f(\s|$)`),
		regexp.MustCompile(`\bchown\s+-R\s+\S+:\S+\s+\x2f(\s|$)`),
		// raw block-device writes (does NOT match /dev/null)
		regexp.MustCompile(`\bdd\b[^\n]*of=\x2fdev\x2f(sd|nvme|hd|vd|xvd|mmcblk)`),
	}
	safeRedir = regexp.MustCompile(`[0-9&]?>>?\s*\x2fdev\x2f(null|stdout|stderr|zero)\b|[0-9&]?>&[0-9-]`)

	// ---- read-only mode: first-word allowlist ----
	readOnlyFirsts = map[string]bool{
		"cat": true, "ls": true, "find": true, "grep": true, "rg": true,
		"head": true, "tail": true, "wc": true, "tree": true, "file": true,
		"stat": true, "pwd": true, "which": true, "type": true, "date": true,
		"uname": true, "whoami": true, "id": true, "echo": true, "printf": true,
		"diff": true, "cmp": true, "sort": true, "uniq": true, "tr": true,
		"cut": true, "column": true, "jq": true, "realpath": true,
		"readlink": true, "basename": true, "dirname": true, "du": true,
		"df": true, "free": true, "uptime": true, "ps": true, "top": true,
		"htop": true, "less": true, "more": true, "strings": true, "xxd": true,
		"od": true, "md5sum": true, "sha1sum": true, "sha256sum": true,
		"base64": true, "sed": true, "awk": true, "gawk": true, "seq": true,
		"numfmt": true,
	}

	readOnlyGitSubs = map[string]bool{
		"log": true, "diff": true, "status": true, "show": true,
		"branch": true, "tag": true, "remote": true, "blame": true,
		"shortlog": true, "describe": true, "rev-parse": true, "rev-list": true,
		"ls-files": true, "ls-tree": true, "cat-file": true, "name-rev": true,
		"whatchanged": true, "grep": true, "annotate": true, "reflog": true,
		"version": true, "config": true,
	}
}

// -------- HTTP helpers --------

func originAllowed(o string) bool {
	if o == "" {
		return true // same-origin / non-browser clients (curl, userscript GM_xmlhttpRequest)
	}
	for _, a := range allowedOrigins {
		if a == o {
			return true
		}
	}
	return false
}

func cors(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	// Only echo an Origin we explicitly trust. Never "*", because with
	// credentials and custom headers that would let any site drive the shell.
	if origin != "" && originAllowed(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	} else if origin != "" {
		// Disallowed origin: no CORS headers -> browser blocks the response.
		return
	}
	// Reflect only the headers the client actually asks for (preflight).
	if reqHeaders := r.Header.Get("Access-Control-Request-Headers"); reqHeaders != "" {
		w.Header().Set("Access-Control-Allow-Headers", reqHeaders)
	} else {
		w.Header().Set("Access-Control-Allow-Headers",
			"Authorization, Content-Type, ngrok-skip-browser-warning, X-Requested-With, X-Ronin-Confirm, X-Ronin-Read-Only, X-Ronin-MCP-Approval")
	}
	w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
	w.Header().Set("Access-Control-Max-Age", "86400")
}

type execRequest struct {
	Command string `json:"command"`
	Root    string `json:"root,omitempty"`
}

// rootAllowed reports whether abs is inside one of the permitted roots.
// Symlinks are resolved first so a link can't escape confinement.
func rootAllowed(abs string) bool {
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		real = abs
	}
	roots := allowRoots
	if len(roots) == 0 {
		// No explicit allowlist: confine everything to the configured root.
		roots = []string{defaultRoot}
	}
	for _, allowed := range roots {
		ar, err := filepath.EvalSymlinks(allowed)
		if err != nil {
			ar = allowed
		}
		if real == ar || strings.HasPrefix(real, ar+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func resolveRoot(clientRoot string) (string, error) {
	root := strings.TrimSpace(clientRoot)
	if root == "" {
		return defaultRoot, nil
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if !rootAllowed(abs) {
		return "", os.ErrPermission
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", os.ErrInvalid
	}
	return abs, nil
}

// -------- security: classification --------

type decision struct {
	Allowed      bool
	NeedsConfirm bool
	Reason       string
}

func classifyCommand(cmdStr string, alreadyConfirmed bool, clientReadOnly bool) decision {
	// 1. Hard blocklist: always rejected.
	for _, re := range hardBlocklist {
		if re.MatchString(cmdStr) {
			return decision{Allowed: false, Reason: "blocked by server hard-blocklist"}
		}
	}

	// 2. Read-only mode: server-wide or per-request.
	if readOnly || clientReadOnly {
		if !isReadOnlyCommand(cmdStr) {
			return decision{Allowed: false, Reason: "read-only mode: command would modify state"}
		}
	}

	// 3. Confirm list: requires X-Ronin-Confirm: yes.
	for _, re := range confirmPatterns {
		if re.MatchString(cmdStr) {
			if alreadyConfirmed {
				return decision{Allowed: true}
			}
			return decision{Allowed: true, NeedsConfirm: true, Reason: "server requires explicit confirmation"}
		}
	}

	return decision{Allowed: true}
}

// splitShellSegments splits a command on ; | || & && so we can inspect
// each segment's first word independently.
func splitShellSegments(s string) []string {
	var out []string
	var cur strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		if c == ';' {
			out = append(out, cur.String())
			cur.Reset()
			i++
			continue
		}
		if c == '|' {
			if i+1 < len(s) && s[i+1] == '|' {
				i += 2
			} else {
				i++
			}
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		if c == '&' {
			if i+1 < len(s) && s[i+1] == '&' {
				i += 2
			} else {
				i++
			}
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
		i++
	}
	out = append(out, cur.String())
	return out
}

func isReadOnlyCommand(cmdStr string) bool {
	s := strings.TrimSpace(cmdStr)
	if s == "" {
		return false
	}

	// Output redirection is a write UNLESS it targets a safe sink
	// (null/zero/stdout/stderr devices, or a descriptor dup like 2>&1).
	if strings.Contains(safeRedir.ReplaceAllString(s, ""), ">") {
		return false
	}

	for _, seg := range splitShellSegments(s) {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		fields := strings.Fields(seg)
		if len(fields) == 0 {
			continue
		}

		first := fields[0]

		// git: only read-only subcommands allowed.
		if first == "git" && len(fields) > 1 {
			if readOnlyGitSubs[fields[1]] {
				continue
			}
			return false
		}

		if !readOnlyFirsts[first] {
			return false
		}

		// find: block destructive actions.
		if first == "find" {
			for _, f := range fields {
				if f == "-delete" || f == "-exec" || f == "-execdir" || f == "-ok" || f == "-okdir" {
					return false
				}
			}
		}

		// sed: block in-place edits.
		if first == "sed" {
			for _, f := range fields {
				if f == "-i" || strings.HasPrefix(f, "--in-place") {
					return false
				}
			}
		}

		// awk/gawk: block inplace extension.
		if first == "awk" || first == "gawk" {
			for _, f := range fields {
				if strings.Contains(f, "inplace") {
					return false
				}
			}
		}
	}

	return true
}

// -------- /exec handler --------

func handleExec(w http.ResponseWriter, r *http.Request) {
	cors(w, r)
	if r.Method == http.MethodOptions {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if authToken != "" && r.Header.Get("Authorization") != "Bearer "+authToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Command == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// ----- security gate -----
	confirmed := r.Header.Get("X-Ronin-Confirm") == "yes"
	clientReadOnly := r.Header.Get("X-Ronin-Read-Only") == "yes"

	d := classifyCommand(req.Command, confirmed, clientReadOnly)
	if !d.Allowed {
		log.Printf("blocked: %s (%s)", d.Reason, firstLine(req.Command))
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "blocked", "data": d.Reason + "\n"})
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "exit", "code": 126})
		return
	}
	if d.NeedsConfirm {
		log.Printf("confirm required: %s", firstLine(req.Command))
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "confirm_required", "data": d.Reason + "\n"})
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "exit", "code": 125})
		return
	}

	// ----- run -----
	workDir, err := resolveRoot(req.Root)
	if err != nil {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "data": "invalid root: " + err.Error() + "\n"})
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "exit", "code": -1})
		return
	}

	tmpScript, err := os.CreateTemp("", "ronin-*.sh")
	if err != nil {
		http.Error(w, "temp: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmpScript.Name())

	cwdFile, err := os.CreateTemp("", "ronin-cwd-*")
	if err != nil {
		http.Error(w, "temp: "+err.Error(), http.StatusInternalServerError)
		return
	}
	cwdFile.Close()
	defer os.Remove(cwdFile.Name())

	prelude := `trap '__ck_rc=$?; pwd > "$__RONIN_CWD_FILE" 2>/dev/null; exit $__ck_rc' EXIT
`
	if _, err := tmpScript.WriteString(prelude + req.Command + "\n"); err != nil {
		http.Error(w, "write: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tmpScript.Close()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)

	var writeMu sync.Mutex
	emit := func(v any) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = enc.Encode(v)
		if flusher != nil {
			flusher.Flush()
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", tmpScript.Name())
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"RONIN=1",
		"__RONIN_CWD_FILE="+cwdFile.Name(),
		"RONIN_CONFIG="+configFilePath,
		"RONIN_MCP_APPROVAL="+r.Header.Get("X-Ronin-MCP-Approval"),
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		emit(map[string]any{"type": "error", "data": err.Error()})
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		emit(map[string]any{"type": "error", "data": err.Error()})
		return
	}
	if err := cmd.Start(); err != nil {
		emit(map[string]any{"type": "error", "data": err.Error()})
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	stream := func(r io.Reader) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			emit(map[string]any{"type": "output", "data": sc.Text() + "\n"})
		}
	}
	go stream(stdout)
	go stream(stderr)
	wg.Wait()

	code := 0
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	if ctx.Err() == context.DeadlineExceeded {
		emit(map[string]any{"type": "error", "data": "timeout\n"})
	}

	if data, err := os.ReadFile(cwdFile.Name()); err == nil {
		newCwd := strings.TrimSpace(string(data))
		if newCwd != "" {
			emit(map[string]any{"type": "cwd", "data": newCwd})
		}
	}

	emit(map[string]any{"type": "exit", "code": code})
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// -------- ngrok --------

func ngrokBinaryName() string {
	if runtime.GOOS == "windows" {
		return "ngrok.exe"
	}
	return "ngrok"
}

func ngrokDownloadURL() (string, error) {
	osName := runtime.GOOS
	arch := runtime.GOARCH

	var ext string
	switch osName {
	case "linux", "darwin":
		ext = "tgz"
	case "windows":
		ext = "zip"
	default:
		return "", fmt.Errorf("unsupported OS for ngrok: %s", osName)
	}

	switch arch {
	case "amd64", "arm64", "386":
	default:
		return "", fmt.Errorf("unsupported arch for ngrok: %s", arch)
	}

	return fmt.Sprintf(
		"https://bin.equinox.io/c/bNyj1mQVY4c/ngrok-v3-stable-%s-%s.%s",
		osName, arch, ext,
	), nil
}

func extractTarGz(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		out := filepath.Join(dest, filepath.Base(hdr.Name))
		of, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(of, tr); err != nil {
			of.Close()
			return err
		}
		of.Close()
	}
	return nil
}

func extractZip(archive, dest string) error {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		out := filepath.Join(dest, filepath.Base(f.Name))
		rc, err := f.Open()
		if err != nil {
			return err
		}
		of, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			rc.Close()
			return err
		}
		if _, err := io.Copy(of, rc); err != nil {
			of.Close()
			rc.Close()
			return err
		}
		of.Close()
		rc.Close()
	}
	return nil
}

// ensureNgrok downloads & extracts the ngrok binary into dir (if not present).
func ensureNgrok(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	binPath := filepath.Join(dir, ngrokBinaryName())
	if st, err := os.Stat(binPath); err == nil && st.Size() > 0 {
		log.Printf("ngrok: using cached binary %s", binPath)
		return binPath, nil
	}

	url, err := ngrokDownloadURL()
	if err != nil {
		return "", err
	}
	log.Printf("ngrok: downloading %s", url)

	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ngrok download failed: %s", resp.Status)
	}

	tmp, err := os.CreateTemp("", "ngrok-archive-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return "", err
	}
	tmp.Close()

	if runtime.GOOS == "windows" {
		if err := extractZip(tmp.Name(), dir); err != nil {
			return "", err
		}
	} else {
		if err := extractTarGz(tmp.Name(), dir); err != nil {
			return "", err
		}
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(binPath, 0o755); err != nil {
			return "", err
		}
	}
	log.Printf("ngrok: installed %s", binPath)
	return binPath, nil
}

// startNgrok launches `ngrok http <port> --authtoken ... [--domain ...]`.
func startNgrok(binPath string, port int, token, domain string) (*exec.Cmd, error) {
	args := []string{
		"http", fmt.Sprintf("%d", port),
		"--authtoken", token,
		"--log", "stdout",
		"--log-level", "warn",
	}
	if domain != "" {
		args = append(args, "--domain", domain)
	}

	cmd := exec.Command(binPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// pollNgrokURL queries the local ngrok API until the public URL is known.
func pollNgrokURL(ctx context.Context) {
	type tunnel struct {
		PublicURL string `json:"public_url"`
		Proto     string `json:"proto"`
	}
	type apiResp struct {
		Tunnels []tunnel `json:"tunnels"`
	}

	client := &http.Client{Timeout: 3 * time.Second}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	apiURL := "http://" + ngrokWebAddr + "/api/tunnels"

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		resp, err := client.Get(apiURL)
		if err != nil {
			continue
		}
		var ar apiResp
		decErr := json.NewDecoder(resp.Body).Decode(&ar)
		resp.Body.Close()
		if decErr != nil {
			continue
		}

		// Prefer https; fall back to the first tunnel.
		var pick string
		for _, t := range ar.Tunnels {
			if strings.HasPrefix(t.PublicURL, "https://") {
				pick = t.PublicURL
				break
			}
			if pick == "" {
				pick = t.PublicURL
			}
		}
		if pick == "" {
			continue
		}

		ngrokMu.Lock()
		if ngrokURL != pick {
			ngrokURL = pick
			log.Printf("ngrok: public url = %s", pick)
		}
		ngrokMu.Unlock()
	}
}

func handleNgrokInfo(w http.ResponseWriter, r *http.Request) {
	cors(w, r)
	if r.Method == http.MethodOptions {
		return
	}
	if authToken != "" && r.Header.Get("Authorization") != "Bearer "+authToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ngrokMu.RLock()
	u := ngrokURL
	ngrokMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"enabled": cfg.Ngrok.Enabled,
		"url":     u,
		"port":    cfg.Port,
		"domain":  cfg.Ngrok.Domain,
	})
}

// handleInfo exposes read-only server metadata (used by the client's
// security badge and Test button).
func handleInfo(w http.ResponseWriter, r *http.Request) {
	cors(w, r)
	if r.Method == http.MethodOptions {
		return
	}
	if authToken != "" && r.Header.Get("Authorization") != "Bearer "+authToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"read_only":        readOnly,
		"hard_blocklist":   len(hardBlocklist),
		"confirm_patterns": len(confirmPatterns),
		"root":             defaultRoot,
		"port":             cfg.Port,
		"version":          "1.0.0",
	})
}

// -------- main --------

func main() {
	configPath := flag.String("config", defaultConfigPath(), "path to config.json")
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		os.Exit(runMCP(os.Args[2:]))
	}
	flag.Parse()

	if _, err := os.Stat(*configPath); os.IsNotExist(err) {
		if err := writeDefaultConfig(*configPath); err != nil {
			log.Fatalf("write default config: %v", err)
		}
		log.Printf("wrote default config to %s", *configPath)
		log.Printf("edit it (set root, token, ngrok.authtoken, ...) then run ./ronin again")
		return
	}

	loaded, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	cfg = loaded

	// timeout
	if cfg.Timeout != "" {
		d, err := time.ParseDuration(cfg.Timeout)
		if err != nil {
			log.Fatalf("bad timeout %q: %v", cfg.Timeout, err)
		}
		timeout = d
	} else {
		timeout = 5 * time.Minute
	}

	// root
	abs, err := filepath.Abs(cfg.Root)
	if err != nil {
		log.Fatalf("bad root %q: %v", cfg.Root, err)
	}
	defaultRoot = abs

	// allow-roots
	for _, p := range cfg.AllowRoots {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		a, err := filepath.Abs(p)
		if err != nil {
			log.Fatalf("bad allow_root %q: %v", p, err)
		}
		allowRoots = append(allowRoots, a)
	}

	authToken = cfg.Token
	if abs, err := filepath.Abs(*configPath); err == nil {
		configFilePath = abs
	} else {
		configFilePath = *configPath
	}
	readOnly = cfg.ReadOnly
	allowedOrigins = cfg.AllowedOrigins

	log.Printf("config=%s port=%d root=%s allow=%v timeout=%s auth=%v read-only=%v ngrok=%v",
		*configPath, cfg.Port, defaultRoot, allowRoots, timeout, authToken != "", readOnly, cfg.Ngrok.Enabled)
	if authToken == "" {
		log.Printf("WARNING: no auth token set — the server is reachable by anyone who can connect. Set \"token\" in config.json.")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if cfg.Ngrok.Enabled {
		if cfg.Ngrok.Authtoken == "" {
			log.Fatal("ngrok.enabled=true but ngrok.authtoken is empty")
		}

		binPath, err := ensureNgrok(cfg.Ngrok.DataDir)
		if err != nil {
			log.Fatalf("ngrok: %v", err)
		}

		cmd, err := startNgrok(binPath, cfg.Port, cfg.Ngrok.Authtoken, cfg.Ngrok.Domain)
		if err != nil {
			log.Fatalf("ngrok start: %v", err)
		}
		ngrokCmd = cmd

		go pollNgrokURL(ctx)
		go func() {
			<-ctx.Done()
			if ngrokCmd != nil && ngrokCmd.Process != nil {
				log.Printf("ngrok: shutting down")
				_ = ngrokCmd.Process.Signal(syscall.SIGTERM)
			}
		}()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/exec", handleExec)
	mux.HandleFunc("/ngrok", handleNgrokInfo)
	mux.HandleFunc("/info", handleInfo)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		cors(w, r)
		if r.Method == http.MethodOptions {
			return
		}
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = srv.Shutdown(shutCtx)
	}()

	log.Printf("listening on :%d", cfg.Port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
