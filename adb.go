package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// An MCP server exposing Android device control over stdio, so the same
// capability works from Ronin and from any other MCP host.
//
// Design choices worth stating:
//
//   - Shell access is a first-class tool but it is NOT unrestricted. The
//     first word must be on an allowlist, and any shell metacharacter that
//     would chain, redirect or substitute is rejected before adb is run.
//     A model that can run arbitrary shell on a phone can do anything the
//     shell user can, so the allowlist is the whole safety story.
//   - Every tool takes explicit, named arguments. Nothing interpolates a
//     free-form string into a command line except the pieces that are
//     validated.
//   - There is no flashing, no bootloader mode, no root toggle. Those are
//     destructive and unrecoverable remotely, so they stay manual.

const (
	adbDefault    = "adb"
	adbCmdTimeout = 60 * time.Second
	adbMaxOutput  = 256 * 1024
)

// shellAllowlist is the set of first words adb.shell may execute. It is
// deliberately read-mostly: anything that installs, removes, or changes
// system state has a dedicated tool with its own checks.
var shellAllowlist = map[string]bool{
	"ls": true, "cat": true, "echo": true, "printf": true,
	"getprop": true, "setprop": false,
	"ps": true, "top": true, "id": true, "whoami": true,
	"uname": true, "date": true, "uptime": true, "df": true, "du": true,
	"which": true, "stat": true, "wc": true, "head": true, "tail": true,
	"grep": true, "find": true, "dumpsys": true, "cmd": true,
	"pm": true, "am": true, "settings": true, "wm": true,
	"logcat": true, "service": true, "content": true,
}

// shellDeniedSub describes subcommands that the allowlisted tools must
// still not perform through adb.shell; they have dedicated tools or are
// destructive.
var shellDeniedSub = []string{
	"pm uninstall", "pm clear", "pm disable", "pm enable", "pm hide",
	"pm suspend", "pm force-stop", "am force-stop", "am kill",
	"content delete", "content update", "content insert",
	"settings put", "settings delete", "wm size reset", "wm density reset",
}

// unsafeShellChars are rejected anywhere in an adb.shell command. They are
// how a single allowlisted word becomes a pipeline.
var unsafeShellChars = []string{";", "|", "&", ">", "<", "`", "$(", "${", "\n", "\r"}

func adbBin() string {
	if v := os.Getenv("RONIN_ADB"); v != "" {
		return v
	}
	if v := os.Getenv("ADB"); v != "" {
		return v
	}
	return adbDefault
}

func adbSerial() string { return strings.TrimSpace(os.Getenv("RONIN_ADB_SERIAL")) }

func adbArgs(args ...string) []string {
	if s := adbSerial(); s != "" {
		return append([]string{"-s", s}, args...)
	}
	return args
}

func adbRun(args ...string) (string, error) {
	cmd := exec.Command(adbBin(), adbArgs(args...)...)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(adbCmdTimeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return "", fmt.Errorf("adb timed out after %s", adbCmdTimeout)
	}
	s := string(out)
	if len(s) > adbMaxOutput {
		s = s[:adbMaxOutput] + "\n[output truncated]"
	}
	return s, err
}

func adbShell(command string) (string, error) {
	return adbRun(append([]string{"shell"}, command)...)
}

// validateShellCommand is the gate for adb.shell. Order matters: reject
// metacharacters first so nothing downstream has to reason about chaining,
// then check the first word, then check denied subcommand pairs.
func validateShellCommand(cmd string) error {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return fmt.Errorf("empty command")
	}
	for _, bad := range unsafeShellChars {
		if strings.Contains(cmd, bad) {
			return fmt.Errorf("refusing command containing %q: shell chaining and redirection are not permitted", bad)
		}
	}
	fields := strings.Fields(cmd)
	first := fields[0]
	allowed, known := shellAllowlist[first]
	if !known {
		return fmt.Errorf("%q is not on the shell allowlist; use a dedicated tool or ask the user", first)
	}
	if !allowed {
		return fmt.Errorf("%q is explicitly denied", first)
	}
	lower := strings.ToLower(cmd)
	for _, denied := range shellDeniedSub {
		if strings.HasPrefix(lower, denied) {
			return fmt.Errorf("refusing %q: state-changing subcommand, use the dedicated tool", denied)
		}
	}
	return nil
}

// -------- tools --------

type adbTool struct {
	Name        string
	Description string
	Schema      map[string]any
	Run         func(map[string]any) (string, error)
}

func strArg(a map[string]any, k string) string {
	if v, ok := a[k].(string); ok {
		return v
	}
	return ""
}

func requireArg(a map[string]any, k string) (string, error) {
	v := strArg(a, k)
	if strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("missing required argument %q", k)
	}
	return v, nil
}

func requirePhonePath(a map[string]any, k string) (string, error) {
	p, err := requireArg(a, k)
	if err != nil {
		return "", err
	}
	if strings.Contains(p, "..") {
		return "", fmt.Errorf("path may not contain ..")
	}
	return p, nil
}

func objSchema(props map[string]any, required ...string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func validPackageName(p string) bool {
	if p == "" || strings.HasPrefix(p, ".") || strings.HasSuffix(p, ".") {
		return false
	}
	for _, r := range p {
		if !(r == '.' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return strings.Contains(p, ".")
}

func adbTools() []adbTool {
	return []adbTool{
		{"adb.devices", "List attached devices with state and model.", objSchema(nil),
			func(a map[string]any) (string, error) { return adbRun("devices", "-l") }},
		{"adb.device_info", "Summarise the connected device: model, android version, sdk, abi, fingerprint.", objSchema(nil),
			func(a map[string]any) (string, error) {
				props := []string{
					"ro.product.model", "ro.product.device", "ro.product.manufacturer",
					"ro.build.version.release", "ro.build.version.sdk",
					"ro.product.cpu.abi", "ro.build.fingerprint", "ro.debuggable",
				}
				var b strings.Builder
				for _, p := range props {
					out, _ := adbShell("getprop " + p)
					fmt.Fprintf(&b, "%-32s %s\n", p, strings.TrimSpace(out))
				}
				return b.String(), nil
			}},
		{"adb.shell", "Run a read-mostly shell command on the device. First word must be allowlisted; shell metacharacters are rejected.", objSchema(map[string]any{
			"command": map[string]any{"type": "string", "description": "e.g. getprop ro.product.model, ls /sdcard, dumpsys battery"},
		}, "command"),
			func(a map[string]any) (string, error) {
				cmd, err := requireArg(a, "command")
				if err != nil {
					return "", err
				}
				if err := validateShellCommand(cmd); err != nil {
					return "", err
				}
				return adbShell(cmd)
			}},
		{"adb.getprop", "Read a single system property.", objSchema(map[string]any{
			"key": map[string]any{"type": "string", "description": "e.g. ro.build.version.sdk"},
		}, "key"),
			func(a map[string]any) (string, error) {
				k, err := requireArg(a, "key")
				if err != nil {
					return "", err
				}
				if strings.ContainsAny(k, " ;|&><`$") {
					return "", fmt.Errorf("invalid property name")
				}
				return adbShell("getprop " + k)
			}},
		{"adb.list_packages", "List installed packages, optionally filtered by substring.", objSchema(map[string]any{
			"filter":           map[string]any{"type": "string", "description": "substring, e.g. camera"},
			"third_party_only": map[string]any{"type": "boolean", "description": "only user-installed apps"},
		}),
			func(a map[string]any) (string, error) {
				args := "pm list packages"
				if b, _ := a["third_party_only"].(bool); b {
					args += " -3"
				}
				out, err := adbShell(args)
				if err != nil {
					return out, err
				}
				f := strings.ToLower(strArg(a, "filter"))
				if f == "" {
					return out, nil
				}
				var keep []string
				for _, line := range strings.Split(out, "\n") {
					if strings.Contains(strings.ToLower(line), f) {
						keep = append(keep, line)
					}
				}
				return strings.Join(keep, "\n"), nil
			}},
		{"adb.package_info", "Dump metadata for one package.", objSchema(map[string]any{
			"package": map[string]any{"type": "string", "description": "e.g. com.android.camera"},
		}, "package"),
			func(a map[string]any) (string, error) {
				p, err := requireArg(a, "package")
				if err != nil {
					return "", err
				}
				if !validPackageName(p) {
					return "", fmt.Errorf("invalid package name")
				}
				return adbShell("dumpsys package " + p)
			}},
		{"adb.install", "Install an APK from the host filesystem.", objSchema(map[string]any{
			"apk_path": map[string]any{"type": "string"},
			"replace":  map[string]any{"type": "boolean", "description": "reinstall if present (-r)"},
		}, "apk_path"),
			func(a map[string]any) (string, error) {
				p, err := requireArg(a, "apk_path")
				if err != nil {
					return "", err
				}
				if !strings.HasSuffix(strings.ToLower(p), ".apk") {
					return "", fmt.Errorf("not an .apk file")
				}
				if _, err := os.Stat(p); err != nil {
					return "", fmt.Errorf("host file not readable: %w", err)
				}
				args := []string{"install"}
				if b, _ := a["replace"].(bool); b {
					args = append(args, "-r")
				}
				return adbRun(append(args, p)...)
			}},
		{"adb.uninstall", "Uninstall a package. Destructive: removes app data unless keep_data is set.", objSchema(map[string]any{
			"package":   map[string]any{"type": "string"},
			"keep_data": map[string]any{"type": "boolean", "description": "keep app data (-k)"},
		}, "package"),
			func(a map[string]any) (string, error) {
				p, err := requireArg(a, "package")
				if err != nil {
					return "", err
				}
				if !validPackageName(p) {
					return "", fmt.Errorf("invalid package name")
				}
				args := []string{"uninstall"}
				if b, _ := a["keep_data"].(bool); b {
					args = append(args, "-k")
				}
				return adbRun(append(args, p)...)
			}},
		{"adb.push", "Copy a host file to the device.", objSchema(map[string]any{
			"local":  map[string]any{"type": "string"},
			"remote": map[string]any{"type": "string", "description": "e.g. /sdcard/Download/x.txt"},
		}, "local", "remote"),
			func(a map[string]any) (string, error) {
				l, err := requireArg(a, "local")
				if err != nil {
					return "", err
				}
				r, err := requirePhonePath(a, "remote")
				if err != nil {
					return "", err
				}
				if _, err := os.Stat(l); err != nil {
					return "", fmt.Errorf("host file not readable: %w", err)
				}
				return adbRun("push", l, r)
			}},
		{"adb.pull", "Copy a file from the device to the host.", objSchema(map[string]any{
			"remote": map[string]any{"type": "string"},
			"local":  map[string]any{"type": "string", "description": "host destination directory"},
		}, "remote", "local"),
			func(a map[string]any) (string, error) {
				r, err := requirePhonePath(a, "remote")
				if err != nil {
					return "", err
				}
				l, err := requireArg(a, "local")
				if err != nil {
					return "", err
				}
				return adbRun("pull", r, l)
			}},
		{"adb.logcat", "Read recent logcat output, optionally filtered by substring.", objSchema(map[string]any{
			"filter": map[string]any{"type": "string"},
			"lines":  map[string]any{"type": "number", "description": "default 200"},
		}),
			func(a map[string]any) (string, error) {
				n := 200
				if f, ok := a["lines"].(float64); ok && f > 0 && f < 5000 {
					n = int(f)
				}
				out, err := exec.Command(adbBin(), adbArgs("-d", "logcat")...).Output()
				if err != nil {
					return "", fmt.Errorf("logcat: %w", err)
				}
				lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
				if len(lines) > n {
					lines = lines[len(lines)-n:]
				}
				joined := strings.Join(lines, "\n")
				if f := strings.ToLower(strArg(a, "filter")); f != "" {
					var keep []string
					for _, l := range lines {
						if strings.Contains(strings.ToLower(l), f) {
							keep = append(keep, l)
						}
					}
					joined = strings.Join(keep, "\n")
				}
				if len(joined) > adbMaxOutput {
					joined = joined[:adbMaxOutput] + "\n[truncated]"
				}
				return joined, nil
			}},
		{"adb.screenshot", "Capture the screen and save it on the host, returning the path.", objSchema(map[string]any{
			"out_path": map[string]any{"type": "string", "description": "host destination .png"},
		}, "out_path"),
			func(a map[string]any) (string, error) {
				out, err := requireArg(a, "out_path")
				if err != nil {
					return "", err
				}
				if !strings.HasSuffix(strings.ToLower(out), ".png") {
					return "", fmt.Errorf("out_path must end in .png")
				}
				if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
					return "", err
				}
				raw, err := adbRun("exec-out", "screencap", "-p")
				if err != nil {
					return "", fmt.Errorf("screencap: %w", err)
				}
				if err := os.WriteFile(out, []byte(raw), 0o600); err != nil {
					return "", err
				}
				return fmt.Sprintf("screenshot saved to %s (%d bytes)", out, len(raw)), nil
			}},
		{"adb.tap", "Tap a screen coordinate.", objSchema(map[string]any{
			"x": map[string]any{"type": "number"}, "y": map[string]any{"type": "number"},
		}, "x", "y"),
			func(a map[string]any) (string, error) {
				x, okx := a["x"].(float64)
				y, oky := a["y"].(float64)
				if !okx || !oky {
					return "", fmt.Errorf("x and y are required numbers")
				}
				return adbShell(fmt.Sprintf("input tap %d %d", int(x), int(y)))
			}},
		{"adb.swipe", "Swipe between two coordinates.", objSchema(map[string]any{
			"x1": map[string]any{"type": "number"}, "y1": map[string]any{"type": "number"},
			"x2": map[string]any{"type": "number"}, "y2": map[string]any{"type": "number"},
			"duration_ms": map[string]any{"type": "number", "description": "default 300"},
		}, "x1", "y1", "x2", "y2"),
			func(a map[string]any) (string, error) {
				var v [4]int
				for i, k := range []string{"x1", "y1", "x2", "y2"} {
					f, ok := a[k].(float64)
					if !ok {
						return "", fmt.Errorf("missing or invalid %q", k)
					}
					v[i] = int(f)
				}
				d := 300
				if f, ok := a["duration_ms"].(float64); ok && f > 0 {
					d = int(f)
				}
				return adbShell(fmt.Sprintf("input swipe %d %d %d %d %d", v[0], v[1], v[2], v[3], d))
			}},
		{"adb.text", "Type text into the focused field.", objSchema(map[string]any{
			"text": map[string]any{"type": "string"},
		}, "text"),
			func(a map[string]any) (string, error) {
				t, err := requireArg(a, "text")
				if err != nil {
					return "", err
				}
				escaped := strings.ReplaceAll(t, " ", "%s")
				escaped = strings.NewReplacer("'", "", `"`, "").Replace(escaped)
				return adbShell("input text '" + escaped + "'")
			}},
		{"adb.key", "Send a key event by Android keycode name, e.g. KEYCODE_BACK.", objSchema(map[string]any{
			"keycode": map[string]any{"type": "string"},
		}, "keycode"),
			func(a map[string]any) (string, error) {
				k, err := requireArg(a, "keycode")
				if err != nil {
					return "", err
				}
				if !strings.HasPrefix(k, "KEYCODE_") {
					return "", fmt.Errorf("keycode must start with KEYCODE_")
				}
				for _, r := range k {
					if !(r == '_' || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
						return "", fmt.Errorf("invalid keycode")
					}
				}
				return adbShell("input keyevent " + k)
			}},
		{"adb.battery", "Battery status via dumpsys.", objSchema(nil),
			func(a map[string]any) (string, error) { return adbShell("dumpsys battery") }},
		{"adb.screen_info", "Screen size and density.", objSchema(nil),
			func(a map[string]any) (string, error) {
				size, _ := adbShell("wm size")
				dens, _ := adbShell("wm density")
				return size + dens, nil
			}},
		{"adb.start_activity", "Launch an app by package name.", objSchema(map[string]any{
			"package": map[string]any{"type": "string"},
		}, "package"),
			func(a map[string]any) (string, error) {
				p, err := requireArg(a, "package")
				if err != nil {
					return "", err
				}
				if !validPackageName(p) {
					return "", fmt.Errorf("invalid package name")
				}
				return adbShell("monkey -p " + p + " -c android.intent.category.LAUNCHER 1")
			}},
		{"adb.force_stop", "Force-stop a running app.", objSchema(map[string]any{
			"package": map[string]any{"type": "string"},
		}, "package"),
			func(a map[string]any) (string, error) {
				p, err := requireArg(a, "package")
				if err != nil {
					return "", err
				}
				if !validPackageName(p) {
					return "", fmt.Errorf("invalid package name")
				}
				return adbShell("am force-stop " + p)
			}},
	}
}

// -------- MCP stdio server --------

func runMCPAdb(args []string) int {
	serve := false
	for _, a := range args {
		if a == "serve" {
			serve = true
		}
	}
	if !serve {
		fmt.Fprintln(os.Stderr, "usage: ronin mcp-adb serve")
		fmt.Fprintln(os.Stderr, "  speaks MCP over stdio; RONIN_ADB overrides the adb binary, RONIN_ADB_SERIAL picks a device")
		return 2
	}
	tools := adbTools()
	byName := map[string]adbTool{}
	for _, t := range tools {
		byName[t.Name] = t
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	enc := json.NewEncoder(os.Stdout)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		if req.ID == nil {
			continue
		}
		respond := func(result any) {
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		}
		respondErr := func(code int, msg string) {
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": code, "message": msg}})
		}
		switch req.Method {
		case "initialize":
			respond(map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "ronin-adb", "version": "1.0.0"},
			})
		case "tools/list":
			var list []map[string]any
			for _, t := range tools {
				list = append(list, map[string]any{
					"name": t.Name, "description": t.Description, "inputSchema": t.Schema,
				})
			}
			respond(map[string]any{"tools": list})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil {
				respondErr(-32602, "bad params: "+err.Error())
				continue
			}
			t, ok := byName[p.Name]
			if !ok {
				respondErr(-32602, "unknown tool: "+p.Name)
				continue
			}
			if p.Arguments == nil {
				p.Arguments = map[string]any{}
			}
			out, err := t.Run(p.Arguments)
			if err != nil {
				respond(map[string]any{
					"content": []map[string]any{{"type": "text", "text": err.Error()}},
					"isError": true,
				})
				continue
			}
			respond(map[string]any{
				"content": []map[string]any{{"type": "text", "text": out}},
			})
		case "ping":
			respond(map[string]any{})
		default:
			respondErr(-32601, "method not found: "+req.Method)
		}
	}
	return 0
}
