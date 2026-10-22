package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// -------- config --------

// GitHubCfg configures App-only authentication. No PAT: the app signs a
// JWT with its private key and exchanges it for a short-lived installation
// token, so every action runs as <app-slug>[bot].
type GitHubCfg struct {
	AppID          string            `json:"app_id"`
	PrivateKeyPath string            `json:"private_key_path"`
	InstallationID int64             `json:"installation_id"`
	DefaultPolicy  string            `json:"default_policy"`
	Actions        map[string]string `json:"actions,omitempty"`
}

var ghHTTP = &http.Client{Timeout: 30 * time.Second}

// -------- auth --------

func ghB64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func ghJWT(appID string, key *rsa.PrivateKey) (string, error) {
	now := time.Now().Unix()
	hdr := ghB64([]byte(`{"alg":"RS256","typ":"JWT"}`))
	pld := ghB64([]byte(fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":%q}`, now-60, now+600, appID)))
	signing := hdr + "." + pld
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + ghB64(sig), nil
}

func ghLoadKey(path string) (*rsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("no PEM block in %s", path)
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s is not an RSA private key", path)
	}
	return rk, nil
}

func ghAPICall(method, path, jwt, token string, body io.Reader) (int, []byte, error) {
	req, err := http.NewRequest(method, "https://api.github.com"+path, body)
	if err != nil {
		return 0, nil, err
	}
	if jwt != "" {
		req.Header.Set("Authorization", "Bearer "+jwt)
	} else if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := ghHTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

// ghInstallationToken mints a fresh installation token. It is not cached
// across processes (each CLI invocation is a new process); a single mint
// costs one API call and tokens live an hour.
func ghInstallationToken(c Config) (string, error) {
	if c.GitHub.AppID == "" || c.GitHub.PrivateKeyPath == "" {
		return "", fmt.Errorf("github not configured: set github.app_id and github.private_key_path")
	}
	key, err := ghLoadKey(c.GitHub.PrivateKeyPath)
	if err != nil {
		return "", fmt.Errorf("private key: %w", err)
	}
	jwt, err := ghJWT(c.GitHub.AppID, key)
	if err != nil {
		return "", err
	}
	inst := c.GitHub.InstallationID
	if inst == 0 {
		code, body, err := ghAPICall("GET", "/app/installations", jwt, "", nil)
		if err != nil {
			return "", err
		}
		if code/100 != 2 {
			return "", fmt.Errorf("discover installations: http %d %s", code, strings.TrimSpace(string(body)))
		}
		var list []struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return "", err
		}
		if len(list) == 0 {
			return "", fmt.Errorf("no installations found for this app")
		}
		inst = list[0].ID
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", inst)
	code, body, err := ghAPICall("POST", path, jwt, "", nil)
	if err != nil {
		return "", err
	}
	if code/100 != 2 {
		return "", fmt.Errorf("mint token: http %d %s", code, strings.TrimSpace(string(body)))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("empty installation token")
	}
	return out.Token, nil
}

// -------- action table --------

type ghAction struct {
	Name   string
	Method string
	Path   string
	Args   []string
	Body   string // default body when none supplied
	Desc   string
}

func (a ghAction) read() bool { return a.Method == "GET" }

var ghActions = []ghAction{
	// repo
	{"repo.info", "GET", "/repos/{repo}", []string{"repo"}, "", "Repository metadata"},
	{"repo.list", "GET", "/installation/repositories", nil, "", "Repositories the installation can access"},
	{"repo.branches", "GET", "/repos/{repo}/branches", []string{"repo"}, "", "List branches"},
	{"repo.branch.get", "GET", "/repos/{repo}/branches/{branch}", []string{"repo", "branch"}, "", "One branch"},
	{"repo.branch.create", "POST", "/repos/{repo}/git/refs", []string{"repo"}, "", "Create a ref (body: ref, sha)"},
	{"repo.contents.get", "GET", "/repos/{repo}/contents/{path}", []string{"repo", "path"}, "", "Read a file"},
	{"repo.contents.put", "PUT", "/repos/{repo}/contents/{path}", []string{"repo", "path"}, "", "Create/update a file (body: message, content, branch, sha)"},
	{"repo.commits", "GET", "/repos/{repo}/commits", []string{"repo"}, "", "List commits"},
	{"repo.commit.get", "GET", "/repos/{repo}/commits/{sha}", []string{"repo", "sha"}, "", "One commit"},
	{"repo.compare", "GET", "/repos/{repo}/compare/{base}...{head}", []string{"repo", "base", "head"}, "", "Compare two refs"},
	{"repo.tree", "GET", "/repos/{repo}/git/trees/{sha}", []string{"repo", "sha"}, "", "Git tree"},
	{"repo.tags", "GET", "/repos/{repo}/tags", []string{"repo"}, "", "List tags"},
	// pull requests
	{"pr.list", "GET", "/repos/{repo}/pulls", []string{"repo"}, "", "List pull requests"},
	{"pr.get", "GET", "/repos/{repo}/pulls/{number}", []string{"repo", "number"}, "", "One pull request"},
	{"pr.create", "POST", "/repos/{repo}/pulls", []string{"repo"}, "", "Open a PR (body: title, head, base)"},
	{"pr.update", "PATCH", "/repos/{repo}/pulls/{number}", []string{"repo", "number"}, "", "Edit a PR"},
	{"pr.merge", "PUT", "/repos/{repo}/pulls/{number}/merge", []string{"repo", "number"}, `{"merge_method":"merge"}`, "Merge a PR"},
	{"pr.files", "GET", "/repos/{repo}/pulls/{number}/files", []string{"repo", "number"}, "", "Files in a PR"},
	{"pr.commits", "GET", "/repos/{repo}/pulls/{number}/commits", []string{"repo", "number"}, "", "Commits in a PR"},
	{"pr.comments", "GET", "/repos/{repo}/pulls/{number}/comments", []string{"repo", "number"}, "", "Review comments"},
	{"pr.comment", "POST", "/repos/{repo}/pulls/{number}/comments", []string{"repo", "number"}, "", "Add a review comment"},
	{"pr.reviews", "GET", "/repos/{repo}/pulls/{number}/reviews", []string{"repo", "number"}, "", "Reviews"},
	{"pr.review", "POST", "/repos/{repo}/pulls/{number}/reviews", []string{"repo", "number"}, "", "Submit a review"},
	{"pr.checks", "GET", "/repos/{repo}/commits/{sha}/check-runs", []string{"repo", "sha"}, "", "CI check runs for a commit"},
	// issues
	{"issue.list", "GET", "/repos/{repo}/issues", []string{"repo"}, "", "List issues"},
	{"issue.get", "GET", "/repos/{repo}/issues/{number}", []string{"repo", "number"}, "", "One issue"},
	{"issue.create", "POST", "/repos/{repo}/issues", []string{"repo"}, "", "Create an issue"},
	{"issue.update", "PATCH", "/repos/{repo}/issues/{number}", []string{"repo", "number"}, "", "Edit an issue"},
	{"issue.close", "PATCH", "/repos/{repo}/issues/{number}", []string{"repo", "number"}, `{"state":"closed"}`, "Close an issue"},
	{"issue.comment", "POST", "/repos/{repo}/issues/{number}/comments", []string{"repo", "number"}, "", "Comment on an issue"},
	{"issue.comments", "GET", "/repos/{repo}/issues/{number}/comments", []string{"repo", "number"}, "", "Issue comments"},
	{"issue.labels", "GET", "/repos/{repo}/issues/{number}/labels", []string{"repo", "number"}, "", "Issue labels"},
	{"issue.add_labels", "POST", "/repos/{repo}/issues/{number}/labels", []string{"repo", "number"}, "", "Add labels"},
	{"issue.assignees", "GET", "/repos/{repo}/issues/{number}/assignees", []string{"repo", "number"}, "", "Issue assignees"},
	{"issue.milestone", "GET", "/repos/{repo}/issues/{number}/milestone", []string{"repo", "number"}, "", "Issue milestone"},
	// labels and milestones
	{"label.list", "GET", "/repos/{repo}/labels", []string{"repo"}, "", "List labels"},
	{"label.create", "POST", "/repos/{repo}/labels", []string{"repo"}, "", "Create a label"},
	{"milestone.list", "GET", "/repos/{repo}/milestones", []string{"repo"}, "", "List milestones"},
	{"milestone.create", "POST", "/repos/{repo}/milestones", []string{"repo"}, "", "Create a milestone"},
	// releases
	{"release.list", "GET", "/repos/{repo}/releases", []string{"repo"}, "", "List releases"},
	{"release.get", "GET", "/repos/{repo}/releases/{id}", []string{"repo", "id"}, "", "One release"},
	{"release.latest", "GET", "/repos/{repo}/releases/latest", []string{"repo"}, "", "Latest release"},
	{"release.create", "POST", "/repos/{repo}/releases", []string{"repo"}, "", "Create a release"},
	// actions
	{"action.workflows", "GET", "/repos/{repo}/actions/workflows", []string{"repo"}, "", "List workflows"},
	{"action.runs", "GET", "/repos/{repo}/actions/runs", []string{"repo"}, "", "List workflow runs"},
	{"action.trigger", "POST", "/repos/{repo}/actions/workflows/{workflow}/dispatches", []string{"repo", "workflow"}, "", "Dispatch a workflow (body: ref)"},
	{"action.cancel", "POST", "/repos/{repo}/actions/runs/{id}/cancel", []string{"repo", "id"}, "", "Cancel a run"},
	// search
	{"search.repos", "GET", "/search/repositories", nil, "", "Search repositories (--query q=...)"},
	{"search.code", "GET", "/search/code", nil, "", "Search code (--query q=...)"},
	{"search.issues", "GET", "/search/issues", nil, "", "Search issues/PRs (--query q=...)"},
	{"search.users", "GET", "/search/users", nil, "", "Search users (--query q=...)"},
	{"search.commits", "GET", "/search/commits", nil, "", "Search commits (--query q=...)"},
	// user
	{"user.get", "GET", "/users/{user}", []string{"user"}, "", "Public user profile"},
	{"rate.limit", "GET", "/rate_limit", nil, "", "Rate limit status for the installation token"},
}

func findGHAction(name string) (ghAction, bool) {
	for _, a := range ghActions {
		if a.Name == name {
			return a, true
		}
	}
	return ghAction{}, false
}

// -------- policy --------

func (c Config) ghPolicy(name string, read bool) string {
	if p, ok := c.GitHub.Actions[name]; ok && p != "" {
		return p
	}
	if c.GitHub.DefaultPolicy != "" {
		return c.GitHub.DefaultPolicy
	}
	if read {
		return "allow"
	}
	return "confirm"
}

// -------- argument handling --------

var (
	ghRepoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	ghPathRe = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)
	ghAtomRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

func validGHArg(name, v string) bool {
	if v == "" || strings.Contains(v, "..") {
		return false
	}
	switch name {
	case "repo":
		return ghRepoRe.MatchString(v)
	case "path":
		return ghPathRe.MatchString(v)
	default:
		return ghAtomRe.MatchString(v)
	}
}

func buildGHPath(tmpl string, vals map[string]string) string {
	for k, v := range vals {
		tmpl = strings.ReplaceAll(tmpl, "{"+k+"}", v)
	}
	return tmpl
}

func ghApprovalKey(path, body string) string {
	sum := sha256.Sum256([]byte(path + "\x00" + body))
	return hex.EncodeToString(sum[:])
}

func ghPrint(b []byte) {
	var v any
	if json.Unmarshal(b, &v) == nil {
		out, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(string(out))
		return
	}
	fmt.Println(string(b))
}

// -------- dispatch --------

func runGH(args []string) int {
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
		fmt.Fprintln(os.Stderr, "usage: ronin gh [-config path] list")
		fmt.Fprintln(os.Stderr, "       ronin gh [-config path] <action> [args...] [--body JSON] [--query Q]")
		return 2
	}
	c, err := loadConfig(cfgPath)
	if err != nil {
		if rest[0] == "list" {
			c = Config{}
		} else {
			fmt.Fprintln(os.Stderr, "load config:", err)
			return 1
		}
	}
	if rest[0] == "list" {
		return ghList(c)
	}
	return ghRun(c, rest[0], rest[1:])
}

func ghList(c Config) int {
	for _, a := range ghActions {
		fmt.Printf("%-22s %-6s [%s]  %s\n", a.Name, a.Method, c.ghPolicy(a.Name, a.read()), a.Desc)
	}
	return 0
}

func ghRun(c Config, name string, rest []string) int {
	a, ok := findGHAction(name)
	if !ok {
		fmt.Fprintln(os.Stderr, "unknown github action:", name)
		fmt.Fprintln(os.Stderr, "run 'ronin gh list' to see them all")
		return 2
	}
	var pos []string
	var body, query string
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--body":
			if i+1 < len(rest) {
				body = rest[i+1]
				i++
			}
		case "--query":
			if i+1 < len(rest) {
				query = rest[i+1]
				i++
			}
		default:
			pos = append(pos, rest[i])
		}
	}
	if body == "" && len(pos) > len(a.Args) {
		last := strings.TrimSpace(pos[len(pos)-1])
		if strings.HasPrefix(last, "{") {
			body = last
			pos = pos[:len(pos)-1]
		}
	}
	if len(pos) < len(a.Args) {
		fmt.Fprintf(os.Stderr, "%s needs %d arg(s): %v\n", name, len(a.Args), a.Args)
		return 2
	}
	vals := map[string]string{}
	for i, an := range a.Args {
		v := pos[i]
		if !validGHArg(an, v) {
			fmt.Fprintf(os.Stderr, "invalid %s: %q\n", an, v)
			return 2
		}
		vals[an] = v
	}
	path := buildGHPath(a.Path, vals)
	if query != "" {
		path += "?" + query
	}
	if body == "" {
		body = a.Body
	}
	policy := c.ghPolicy(name, a.read())
	key := "gh." + name + "." + ghApprovalKey(path, body)
	switch policy {
	case "deny":
		fmt.Fprintf(os.Stderr, "blocked by GitHub policy: %s is denied\n", name)
		return 126
	case "confirm":
		if os.Getenv("RONIN_GH_APPROVAL") != key {
			fmt.Fprintf(os.Stderr, "[ronin-gh-confirm] action=%s approval=%s\n", name, key)
			return 125
		}
	}
	tok, err := ghInstallationToken(c)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	code, resp, err := ghAPICall(a.Method, path, "", tok, rd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "request:", err)
		return 1
	}
	ghPrint(resp)
	if code/100 != 2 {
		fmt.Fprintf(os.Stderr, "http %d\n", code)
		return 1
	}
	return 0
}
