// Command subctl is the substrate CLI: drive sandboxes from a terminal or
// scripts. Talks to any substrate endpoint — standalone noded or hosted.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const usage = `subctl — substrate CLI

Usage:
  subctl <command> [flags]

Commands:
  sandboxes list                     List sandboxes
  sandboxes create --template T [--user U] [--env K=V]...   Create + wait for running
  sandboxes get ID                   Show one sandbox
  sandboxes destroy ID               Destroy a sandbox
  exec ID -- CMD...                  Run a command, stream output
  fs put ID LOCAL REMOTE             Upload a file
  fs get ID REMOTE LOCAL             Download a file
  fs ls ID DIR                       List a directory
  ports expose ID PORT [--public]    Expose a port; prints preview URL
  tokens mint ID [--scopes s1,s2]    Mint a sandbox-scoped token
  templates list|create              Manage templates
  events [sandbox_id]                Tail lifecycle events
  version                            Print version

Environment:
  SUBSTRATE_URL     API base URL (default http://localhost:8080)
  SUBSTRATE_KEY     API key (sk_... / pk_...)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "subctl:", err)
		os.Exit(1)
	}
}

func dispatch(args []string) error {
	switch args[0] {
	case "version":
		fmt.Println("subctl 1.0.0")
		return nil
	case "sandboxes":
		return sandboxes(args[1:])
	case "exec":
		return runExec(args[1:])
	case "fs":
		return fsCmd(args[1:])
	case "ports":
		return ports(args[1:])
	case "tokens":
		return tokens(args[1:])
	case "templates":
		return templates(args[1:])
	case "events":
		return events(args[1:])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q; try `subctl help`", args[0])
	}
}

func client() (*http.Client, error) { return &http.Client{Timeout: 60 * time.Second}, nil }

func baseURL() string {
	if v := os.Getenv("SUBSTRATE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://localhost:8080"
}

func apiKey() (string, error) {
	k := os.Getenv("SUBSTRATE_KEY")
	if k == "" {
		return "", fmt.Errorf("SUBSTRATE_KEY not set")
	}
	return k, nil
}

// maxRespBytes bounds any single API response read by the CLI.
const maxRespBytes = 64 << 20

func apiCall(method, path string, body io.Reader, idem string) ([]byte, int, error) {
	key, err := apiKey()
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequest(method, baseURL()+path, body)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	c, _ := client()
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	// Overflow-detecting read: a response over the cap must fail loudly.
	// Silent truncation turned `fs get` of any large file into a corrupted
	// local copy with exit code 0.
	out, rerr := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes+1))
	if rerr != nil {
		return nil, resp.StatusCode, fmt.Errorf("%s %s: read response: %w", method, path, rerr)
	}
	if int64(len(out)) > maxRespBytes {
		return nil, resp.StatusCode, fmt.Errorf("%s %s: response exceeds %d MiB limit", method, path, maxRespBytes>>20)
	}
	if resp.StatusCode >= 400 {
		return out, resp.StatusCode, fmt.Errorf("%s %s: %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, resp.StatusCode, nil
}

// idemKey derives a DETERMINISTIC idempotency key from the operation's
// identity: CLI retries of the same logical operation dedupe server-side,
// while different operations never collide. The old wall-clock suffix made
// every retry unique, silently disabling the mechanism it was there for.
func idemKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "cli-" + hex.EncodeToString(sum[:16])
}

func printJSON(b []byte) {
	var v any
	if json.Unmarshal(b, &v) == nil {
		pretty, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(string(pretty))
		return
	}
	fmt.Println(string(b))
}

func flagValue(args []string, name string) (string, []string, bool) {
	for i, a := range args {
		if a == "--"+name && i+1 < len(args) {
			out := append(append([]string{}, args[:i]...), args[i+2:]...)
			return args[i+1], out, true
		}
		if strings.HasPrefix(a, "--"+name+"=") {
			out := append(append([]string{}, args[:i]...), args[i+1:]...)
			return strings.TrimPrefix(a, "--"+name+"="), out, true
		}
	}
	return "", args, false
}

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == "--"+name {
			return true
		}
	}
	return false
}

func sandboxes(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("sandboxes list|create|get|destroy")
	}
	switch args[0] {
	case "list":
		b, _, err := apiCall("GET", "/v1/sandboxes?limit=100", nil, "")
		if err != nil {
			return err
		}
		printJSON(b)
	case "get":
		if len(args) < 2 {
			return fmt.Errorf("usage: sandboxes get ID")
		}
		b, _, err := apiCall("GET", "/v1/sandboxes/"+args[1], nil, "")
		if err != nil {
			return err
		}
		printJSON(b)
	case "create":
		rest := args[1:]
		tpl, rest, _ := flagValue(rest, "template")
		user, rest, _ := flagValue(rest, "user")
		envPairs := []string{}
		for i := 0; i < len(rest); i++ {
			if rest[i] == "--env" && i+1 < len(rest) {
				envPairs = append(envPairs, rest[i+1])
				rest = append(rest[:i], rest[i+2:]...)
				i--
			}
		}
		env := map[string]string{}
		for _, p := range envPairs {
			kv := strings.SplitN(p, "=", 2)
			if len(kv) == 2 {
				env[kv[0]] = kv[1]
			}
		}
		payload := map[string]any{"template_name": tpl, "env": env}
		if user != "" {
			payload["external_user_id"] = user
		}
		jb, _ := json.Marshal(payload)
		b, _, err := apiCall("POST", "/v1/sandboxes", strings.NewReader(string(jb)), idemKey("create"))
		if err != nil {
			return err
		}
		printJSON(b)
	case "destroy":
		if len(args) < 2 {
			return fmt.Errorf("usage: sandboxes destroy ID")
		}
		b, _, err := apiCall("DELETE", "/v1/sandboxes/"+args[1], nil, idemKey("destroy", args[1]))
		if err != nil {
			return err
		}
		printJSON(b)
	default:
		return fmt.Errorf("unknown sandboxes subcommand %q", args[0])
	}
	return nil
}

func runExec(args []string) error {
	if len(args) < 3 || args[0] != "" && args[1] != "--" && args[1] != "" {
	}
	if len(args) < 2 {
		return fmt.Errorf("usage: exec ID -- CMD …")
	}
	sbID := args[0]
	rest := args[1:]
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return fmt.Errorf("no command given")
	}
	payload, _ := json.Marshal(map[string]any{"argv": rest})
	b, _, err := apiCall("POST", "/v1/sandboxes/"+sbID+"/exec", strings.NewReader(string(payload)), idemKey("exec", sbID, string(payload)))
	if err != nil {
		return err
	}
	var resp struct {
		ExecID string `json:"exec_id"`
	}
	json.Unmarshal(b, &resp)
	if resp.ExecID == "" {
		return fmt.Errorf("no exec_id in response: %s", b)
	}

	offset := 0
	lastEvent := ""
	for i := 0; i < 600; i++ {
		sseURL := fmt.Sprintf("/v1/execs/%s/stream?offset=%d", resp.ExecID, offset)
		key, _ := apiKey()
		req, _ := http.NewRequest("GET", baseURL()+sseURL, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		hc := &http.Client{Timeout: 120 * time.Second}
		respHTTP, err := hc.Do(req)
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(respHTTP.Body)
		exited := false
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "id: ") {
				if v, err := strconv.Atoi(strings.TrimPrefix(line, "id: ")); err == nil {
					offset = v
				}
				continue
			}
			if strings.HasPrefix(line, "data: ") && lastEvent == "output" {
				data := strings.TrimPrefix(line, "data: ")
				unescaped := strings.ReplaceAll(data, `\n`, "\n")
				fmt.Print(unescaped)
				if !strings.HasSuffix(unescaped, "\n") {
					fmt.Println()
				}
			}
			if line == "event: exit" {
				exited = true
			}
			if strings.HasPrefix(line, "event: ") {
				lastEvent = strings.TrimPrefix(line, "event: ")
			}
		}
		respHTTP.Body.Close()
		if exited {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("exec stream timed out")
}

func fsCmd(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("fs ls|put|get ID PATH …")
	}
	sub, sbID := args[0], args[1]
	switch sub {
	case "ls":
		dir := "/"
		if len(args) > 2 {
			dir = args[2]
		}
		b, _, err := apiCall("GET", "/v1/sandboxes/"+sbID+"/fs"+fsPath(dir)+"?list=true", nil, "")
		if err != nil {
			return err
		}
		printJSON(b)
	case "put":
		if len(args) < 4 {
			return fmt.Errorf("usage: fs put ID LOCAL REMOTE")
		}
		data, err := os.ReadFile(args[2])
		if err != nil {
			return err
		}
		req, _ := http.NewRequest("PUT", baseURL()+"/v1/sandboxes/"+sbID+"/fs"+fsPath(args[3]), strings.NewReader(string(data)))
		key, _ := apiKey()
		req.Header.Set("Authorization", "Bearer "+key)
		c, _ := client()
		resp, err := c.Do(req)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			return fmt.Errorf("put failed: %d", resp.StatusCode)
		}
		fmt.Printf("%s -> %s (%d bytes)\n", args[2], args[3], len(data))
	case "get":
		if len(args) < 4 {
			return fmt.Errorf("usage: fs get ID REMOTE LOCAL")
		}
		b, _, err := apiCall("GET", "/v1/sandboxes/"+sbID+"/fs"+fsPath(args[2]), nil, "")
		if err != nil {
			return err
		}
		return os.WriteFile(args[3], b, 0o644)
	}
	return nil
}

func ports(args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("ports expose ID PORT [--public]")
	}
	port := args[2]
	public := hasFlag(args, "public")
	payload, _ := json.Marshal(map[string]any{"port": atoi(port), "public": public})
	b, _, err := apiCall("POST", "/v1/sandboxes/"+args[1]+"/ports", strings.NewReader(string(payload)), idemKey("port", port))
	if err != nil {
		return err
	}
	printJSON(b)
	return nil
}

func tokens(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("tokens mint ID [--scopes exec,fs]")
	}
	scopes, _, _ := flagValue(args[1:], "scopes")
	if scopes == "" {
		scopes = "exec,fs,ports"
	}
	payload, _ := json.Marshal(map[string]any{"scopes": strings.Split(scopes, ",")})
	b, _, err := apiCall("POST", "/v1/sandboxes/"+args[1]+"/tokens", strings.NewReader(string(payload)), idemKey("token"))
	if err != nil {
		return err
	}
	printJSON(b)
	return nil
}

func templates(args []string) error {
	if len(args) == 0 || args[0] == "list" {
		b, _, err := apiCall("GET", "/v1/templates", nil, "")
		if err != nil {
			return err
		}
		printJSON(b)
		return nil
	}
	if args[0] == "create" {
		name, _, _ := flagValue(args[1:], "name")
		rootfs, _, _ := flagValue(args[1:], "rootfs")
		memS, _, _ := flagValue(args[1:], "mem-mib")
		mem := atoiOr(memS, 512)
		payload := map[string]any{"name": name, "rootfs_path": rootfs, "mem_mib": mem}
		jb, _ := json.Marshal(payload)
		b, _, err := apiCall("POST", "/v1/templates", strings.NewReader(string(jb)), idemKey("tpl", name))
		if err != nil {
			return err
		}
		printJSON(b)
		return nil
	}
	return fmt.Errorf("templates list|create")
}

func events(args []string) error {
	path := "/v1/events?limit=50"
	if len(args) > 0 {
		path += "&sandbox_id=" + args[0]
	}
	b, _, err := apiCall("GET", path, nil, "")
	if err != nil {
		return err
	}
	printJSON(b)
	return nil
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	return atoi(s)
}

// fsPath escapes a guest-absolute path for URL use, preserving "/" separators
// so names containing "?", "#", or spaces cannot corrupt the request line.
func fsPath(p string) string {
	return strings.ReplaceAll(url.PathEscape(p), "%2F", "/")
}
