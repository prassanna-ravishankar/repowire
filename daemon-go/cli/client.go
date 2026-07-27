package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/proto"
)

type args struct {
	pos   []string
	flags map[string][]string
}

func parse(argv []string, boolFlags ...string) args {
	bools := map[string]bool{}
	for _, name := range boolFlags {
		bools[name] = true
	}
	out := args{flags: map[string][]string{}}
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			out.pos = append(out.pos, arg)
			continue
		}
		name, value := strings.TrimLeft(arg, "-"), ""
		if parts := strings.SplitN(name, "=", 2); len(parts) == 2 {
			name, value = parts[0], parts[1]
		}
		if name == "m" {
			name = "message"
		}
		if strings.HasPrefix(name, "no-") && bools[strings.TrimPrefix(name, "no-")] {
			out.flags[strings.TrimPrefix(name, "no-")] = append(out.flags[strings.TrimPrefix(name, "no-")], "false")
			continue
		}
		if value == "" && !bools[name] && i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
			i++
			value = argv[i]
		}
		if value == "" {
			value = "true"
		}
		out.flags[name] = append(out.flags[name], value)
	}
	return out
}

func (a args) string(name, fallback string) string {
	values := a.flags[name]
	if len(values) > 0 {
		return values[len(values)-1]
	}
	return fallback
}
func (a args) bool(name string) bool {
	value := a.string(name, "false")
	parsed, _ := strconv.ParseBool(value)
	return parsed
}
func (a args) integer(name string, fallback int) int {
	value := a.string(name, "")
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return n
}

type client struct {
	base, token string
	http        *http.Client
}

func newClient() (*client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return &client{base: fmt.Sprintf("http://%s:%d", cfg.Daemon.Host, cfg.Daemon.Port), token: cfg.Daemon.AuthToken, http: &http.Client{}}, nil
}

func (c *client) request(method, path string, body any) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("daemon unavailable at %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	var result map[string]any
	_ = json.Unmarshal(raw, &result)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := strings.TrimSpace(string(raw))
		if result != nil && result["detail"] != nil {
			detail = fmt.Sprint(result["detail"])
		}
		return nil, fmt.Errorf("%s", detail)
	}
	if result == nil {
		result = map[string]any{}
	}
	return result, nil
}

func printJSON(value any) { raw, _ := json.MarshalIndent(value, "", "  "); fmt.Println(string(raw)) }
func stringValue(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	value, _ := m[key].(string)
	return value
}
func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func currentTmuxCircle() string {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return ""
	}
	cfg, err := config.Load()
	if err != nil {
		return ""
	}
	out, err := exec.Command("tmux", "display-message", "-t", pane, "-p", "#{session_name}\t#{window_id}").Output()
	if err != nil {
		return ""
	}
	return tmuxCircleFromOutput(cfg.Daemon.CircleBoundary, string(out))
}

func tmuxCircleFromOutput(boundary proto.CircleBoundary, output string) string {
	parts := strings.Split(strings.TrimSpace(output), "\t")
	if len(parts) != 2 {
		return ""
	}
	return proto.TmuxCircle(boundary, parts[0], parts[1])
}
func anySlice(value any) []any { items, _ := value.([]any); return items }
func addQuery(q url.Values, key, value string) {
	if value != "" {
		q.Set(key, value)
	}
}
func pathQuery(path string, q url.Values) string {
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}
func fatal(err error) int   { fmt.Fprintln(os.Stderr, "repowire:", err); return 1 }
func usage(text string) int { fmt.Fprintln(os.Stderr, "usage: repowire "+text); return 2 }

func parseWhen(raw string) (string, error) {
	value := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "in "))
	if d, err := time.ParseDuration(value); err == nil {
		return time.Now().UTC().Add(d).Format(time.RFC3339), nil
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.UTC().Format(time.RFC3339), nil
	}
	return "", fmt.Errorf("invalid time %q (use RFC3339 or 10m/1h)", raw)
}
