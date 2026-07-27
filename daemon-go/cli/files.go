package cli

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

func runAgents(argv []string) int {
	if len(argv) == 0 || argv[0] != "create" {
		return usage("agents create NAME [--path PATH] [--backend BACKEND] [--force]")
	}
	a := parse(argv[1:], "force", "json")
	if len(a.pos) < 1 {
		return usage("agents create NAME")
	}
	name := a.pos[0]
	if !regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(name) {
		return fatal(fmt.Errorf("invalid agent name %q", name))
	}
	path := a.string("path", "")
	if path == "" {
		root := gitRoot()
		path = filepath.Join(root, ".repowire", "agents", name)
	}
	path = abs(path)
	if _, err := os.Stat(path); err == nil && !a.bool("force") {
		return fatal(fmt.Errorf("%s already exists (pass --force)", path))
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fatal(err)
	}
	agents := fmt.Sprintf("# %s\n\nYou are a durable Repowire worker. Read the assigned job, acknowledge receipt, do the work, and update the job with the current attempt id.\n", name)
	if err := os.WriteFile(filepath.Join(path, "AGENTS.md"), []byte(agents), 0o644); err != nil {
		return fatal(err)
	}
	_ = os.Remove(filepath.Join(path, "CLAUDE.md"))
	if err := os.Symlink("AGENTS.md", filepath.Join(path, "CLAUDE.md")); err != nil {
		_ = os.WriteFile(filepath.Join(path, "CLAUDE.md"), []byte(agents), 0o644)
	}
	backend := a.string("backend", "codex")
	readme := fmt.Sprintf("# %s\n\nCreate a job:\n\n```sh\nrepowire jobs create \"Task\" --path %q --backend %s\n```\n", name, path, backend)
	_ = os.WriteFile(filepath.Join(path, "README.md"), []byte(readme), 0o644)
	if a.bool("json") {
		printJSON(map[string]any{"name": name, "path": path, "backend": backend})
	} else {
		fmt.Println(path)
	}
	return 0
}

func gitRoot() string {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err == nil {
		return strings.TrimSpace(string(out))
	}
	return mustGetwd()
}

func runMemory(argv []string) int {
	if len(argv) == 0 {
		return usage("memory <path|list|show|search|write>")
	}
	a := parse(argv[1:], "all", "append", "force")
	dir, err := memoryDir(a)
	if err != nil {
		return fatal(err)
	}
	switch argv[0] {
	case "path":
		fmt.Println(dir)
		return 0
	case "list":
		entries, _ := filepath.Glob(filepath.Join(dir, "*.md"))
		sort.Strings(entries)
		for _, path := range entries {
			if filepath.Base(path) != "MEMORY.md" {
				fmt.Println(strings.TrimSuffix(filepath.Base(path), ".md"))
			}
		}
		return 0
	case "show":
		if len(a.pos) < 1 {
			return usage("memory show SLUG")
		}
		raw, err := os.ReadFile(filepath.Join(dir, a.pos[0]+".md"))
		if err != nil {
			return fatal(err)
		}
		fmt.Print(string(raw))
		return 0
	case "search":
		if len(a.pos) < 1 {
			return usage("memory search QUERY")
		}
		query := strings.ToLower(strings.Join(a.pos, " "))
		roots := []string{dir}
		if a.bool("all") {
			roots = []string{home(".repowire", "memory"), home(".repowire", "orchestrator", "memory")}
		}
		for _, root := range roots {
			_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() && strings.HasSuffix(path, ".md") {
					raw, _ := os.ReadFile(path)
					for index, line := range strings.Split(string(raw), "\n") {
						if strings.Contains(strings.ToLower(line), query) {
							fmt.Printf("%s:%d %s\n", path, index+1, strings.TrimSpace(line))
							break
						}
					}
				}
				return nil
			})
		}
		return 0
	case "write":
		if len(a.pos) < 1 {
			return usage("memory write SLUG --body BODY")
		}
		slug := a.pos[0]
		if !safeMemoryName(slug) {
			return fatal(fmt.Errorf("slug must match ^[a-zA-Z0-9._-]+$"))
		}
		body := a.string("body", "")
		if body == "" {
			return fatal(fmt.Errorf("--body is required"))
		}
		if a.bool("append") && a.bool("force") {
			return fatal(fmt.Errorf("--append and --force are mutually exclusive"))
		}
		_ = os.MkdirAll(dir, 0o700)
		path := filepath.Join(dir, slug+".md")
		if existing, err := os.ReadFile(path); err == nil {
			if a.bool("append") {
				body = strings.TrimSpace(memoryBody(string(existing))) + "\n\n" + strings.TrimSpace(body)
			} else if !a.bool("force") {
				return fatal(fmt.Errorf("%s exists (pass --append or --force)", path))
			}
		}
		description := a.string("description", "")
		kind := a.string("type", "reference")
		if !safeMemoryName(kind) {
			return fatal(fmt.Errorf("type must match ^[a-zA-Z0-9._-]+$"))
		}
		body = strings.TrimSpace(body)
		if !strings.HasPrefix(body, "# ") {
			title := strings.Title(strings.ReplaceAll(strings.ReplaceAll(slug, "-", " "), "_", " ")) //nolint:staticcheck
			body = "# " + title + "\n\n" + body
		}
		content := fmt.Sprintf("---\nname: %s\ndescription: %s\nmetadata:\n  type: %s\n  updated_at: %s\n---\n\n%s\n", strconv.Quote(slug), strconv.Quote(description), strconv.Quote(kind), strconv.Quote(time.Now().UTC().Format(time.RFC3339)), body)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			return fatal(err)
		}
		refreshMemoryIndex(dir)
		fmt.Println(path)
		return 0
	default:
		return usage("memory <path|list|show|search|write>")
	}
}

func memoryDir(a args) (string, error) {
	scope := a.string("scope", "")
	if scope == "" {
		scope = "user"
		if withinPath(mustGetwd(), home(".repowire", "orchestrator")) {
			scope = "orchestrator"
		}
	}
	root := home(".repowire", "memory")
	switch scope {
	case "global", "user":
		return filepath.Join(root, scope), nil
	case "project", "projects":
		name := first(a.string("project", ""), filepath.Base(mustGetwd()))
		if !safeMemoryName(name) {
			return "", fmt.Errorf("project must match ^[a-zA-Z0-9._-]+$")
		}
		return filepath.Join(root, "projects", name), nil
	case "persona", "personas":
		name := a.string("persona", "")
		if name == "" {
			raw, _ := os.ReadFile(home(".repowire", "orchestrator", "personas", "ACTIVE_PERSONA"))
			name = strings.TrimSpace(string(raw))
		}
		if name == "" {
			return "", fmt.Errorf("persona scope requires --persona or an active persona")
		}
		if !safeMemoryName(name) {
			return "", fmt.Errorf("persona must match ^[a-zA-Z0-9._-]+$")
		}
		return filepath.Join(root, "personas", name), nil
	case "orchestrator":
		target := home(".repowire", "orchestrator", "memory")
		link := filepath.Join(root, "orchestrator")
		_ = os.MkdirAll(root, 0o700)
		if _, err := os.Lstat(link); os.IsNotExist(err) {
			_ = os.Symlink(target, link)
		}
		return target, nil
	default:
		return "", fmt.Errorf("scope must be one of: global, user, project, projects, persona, personas, orchestrator")
	}
}

func safeMemoryName(value string) bool {
	return regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(strings.TrimSpace(value))
}

func withinPath(path, parent string) bool {
	path, _ = filepath.Abs(path)
	parent, _ = filepath.Abs(parent)
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func memoryBody(content string) string {
	if !strings.HasPrefix(content, "---\n") {
		return content
	}
	if index := strings.Index(content[4:], "\n---\n"); index >= 0 {
		return content[4+index+5:]
	}
	return content
}
func refreshMemoryIndex(dir string) {
	entries, _ := filepath.Glob(filepath.Join(dir, "*.md"))
	sort.Strings(entries)
	lines := []string{"# Memory", ""}
	for _, path := range entries {
		base := filepath.Base(path)
		if base != "MEMORY.md" {
			lines = append(lines, "- ["+strings.TrimSuffix(base, ".md")+"]("+base+")")
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

func runOrchestrator(argv []string) int {
	if len(argv) == 0 {
		return usage("orchestrator <init|diff|start|persona>")
	}
	if argv[0] == "persona" {
		return runPersona(argv[1:])
	}
	a := parse(argv[1:], "force", "service")
	switch argv[0] {
	case "init":
		message, err := initOrchestrator(a.bool("force"))
		if err != nil {
			return fatal(err)
		}
		fmt.Println(message)
		return 0
	case "diff":
		return diffOrchestrator()
	case "start":
		workspace := home(".repowire", "orchestrator")
		if _, err := os.Stat(filepath.Join(workspace, "AGENTS.md")); os.IsNotExist(err) {
			if _, err := initOrchestrator(false); err != nil {
				return fatal(err)
			}
		}
		settings := map[string]any{}
		if raw, err := os.ReadFile(filepath.Join(workspace, "orchestrator.yaml")); err == nil {
			_ = yaml.Unmarshal(raw, &settings)
		}
		backend := first(a.string("runtime", ""), stringAny(settings["runtime"]), orchestratorRuntime())
		if backend == "" {
			return fatal(fmt.Errorf("no supported orchestrator runtime found"))
		}
		if a.bool("service") {
			fmt.Fprintln(os.Stderr, "warning: --service is planned; spawning in tmux")
		}
		c, err := newClient()
		if err != nil {
			return fatal(err)
		}
		currentCircle := currentTmuxCircle()
		circle := first(a.string("circle", ""), stringAny(settings["circle"]), currentCircle)
		if circle == "" {
			return fatal(fmt.Errorf("orchestrator start requires a circle in orchestrator.yaml or --circle"))
		}
		body := map[string]any{"path": workspace, "backend": backend, "circle": circle, "role": "orchestrator"}
		if pane := os.Getenv("TMUX_PANE"); pane != "" && circle == currentCircle {
			body["source_pane"] = pane
		}
		if profile := first(a.string("profile", ""), stringAny(settings["profile"])); profile != "" {
			body["profile"] = profile
		}
		if command := stringAny(settings["command"]); command != "" {
			delete(body, "backend")
			body["command"] = command
		}
		result, err := c.request("POST", "/spawn", body)
		if err != nil {
			return fatal(err)
		}
		printJSON(result)
		return 0
	default:
		return usage("orchestrator <init|diff|start|persona>")
	}
}

func initOrchestrator(force bool) (string, error) {
	workspace := home(".repowire", "orchestrator")
	marker := filepath.Join(workspace, "AGENTS.md")
	if _, err := os.Stat(marker); err == nil && !force {
		return "orchestrator already installed at " + workspace, nil
	}
	if _, err := os.Stat(marker); err == nil && force {
		backup := workspace + ".bak." + time.Now().UTC().Format("20060102T150405Z")
		if err := os.Rename(workspace, backup); err != nil {
			return "", fmt.Errorf("back up orchestrator workspace: %w", err)
		}
	}
	root := "assets/orchestrator"
	if err := fs.WalkDir(orchestratorAssets, root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel(root, path)
		target := filepath.Join(workspace, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		raw, err := orchestratorAssets.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o600)
	}); err != nil {
		return "", err
	}
	_ = os.Remove(filepath.Join(workspace, "CLAUDE.md"))
	if err := os.Symlink("AGENTS.md", filepath.Join(workspace, "CLAUDE.md")); err != nil {
		return "", err
	}
	for _, name := range orchestratorSkillNames() {
		link := filepath.Join(workspace, ".claude", "skills", name)
		_ = os.MkdirAll(filepath.Dir(link), 0o700)
		_ = os.RemoveAll(link)
		if err := os.Symlink(filepath.Join("..", "..", ".agents", "skills", name), link); err != nil {
			return "", err
		}
	}
	return "orchestrator workspace created at " + workspace, nil
}

func diffOrchestrator() int {
	workspace := home(".repowire", "orchestrator")
	if _, err := os.Stat(filepath.Join(workspace, "AGENTS.md")); err != nil {
		return fatal(fmt.Errorf("workspace not initialized; run repowire orchestrator init"))
	}
	changed := 0
	paths := append([]string{"AGENTS.md", "orchestrator.yaml.example"}, skillPaths()...)
	for _, rel := range paths {
		shipped, err := orchestratorAssets.ReadFile(filepath.ToSlash(filepath.Join("assets/orchestrator", rel)))
		if err != nil {
			continue
		}
		local, err := os.ReadFile(filepath.Join(workspace, rel))
		status := "missing"
		if err == nil {
			status = "differs"
			if bytes.Equal(local, shipped) {
				status = "identical"
			}
		}
		if status != "identical" {
			changed++
			fmt.Printf("%s: %s\n", rel, status)
		}
	}
	if changed == 0 {
		fmt.Println("orchestrator workspace is up to date")
	}
	return 0
}

func orchestratorSkillNames() []string {
	return []string{"adhd", "cleanup", "coordination", "create-agent", "create-skill", "delegation", "durable-jobs", "handover", "memory", "review-cycle", "worktree-isolation"}
}

func skillPaths() []string {
	paths := make([]string, 0, len(orchestratorSkillNames()))
	for _, name := range orchestratorSkillNames() {
		paths = append(paths, filepath.Join(".agents", "skills", name, "SKILL.md"))
	}
	return paths
}

func orchestratorRuntime() string {
	if runtimeAvailable("pi") && runtimeIntegrated("pi") {
		return "pi"
	}
	for _, name := range []string{"claude-code", "codex", "gemini", "opencode"} {
		if runtimeAvailable(name) {
			return name
		}
	}
	return ""
}

func stringAny(value any) string {
	text, _ := value.(string)
	return text
}

func runPersona(argv []string) int {
	if len(argv) == 0 {
		return usage("orchestrator persona <list|show|path|use|clear>")
	}
	root := home(".repowire", "orchestrator")
	personas := filepath.Join(root, "personas")
	globalPersonas := home(".repowire", "personas")
	active := filepath.Join(personas, "ACTIVE_PERSONA")
	switch argv[0] {
	case "list":
		activeName := strings.TrimSpace(readText(active))
		for _, directory := range []string{personas, globalPersonas} {
			entries, _ := os.ReadDir(directory)
			for _, entry := range entries {
				if entry.IsDir() && safeMemoryName(entry.Name()) {
					marker := " "
					if entry.Name() == activeName {
						marker = "*"
					}
					fmt.Printf("%s %s (%s)\n", marker, entry.Name(), directory)
				}
			}
		}
		return 0
	case "show", "path":
		name := ""
		if len(argv) > 1 {
			name = argv[1]
		} else {
			name = strings.TrimSpace(readText(active))
		}
		path := personaPath(name, personas, globalPersonas)
		if path == "" {
			return fatal(fmt.Errorf("no SOUL.md found for persona %q", name))
		}
		if argv[0] == "path" {
			fmt.Println(path)
			return 0
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return fatal(err)
		}
		fmt.Print(string(raw))
		return 0
	case "use":
		if len(argv) < 2 {
			return usage("orchestrator persona use NAME")
		}
		if !safeMemoryName(argv[1]) {
			return fatal(fmt.Errorf("persona name must match ^[a-zA-Z0-9._-]+$"))
		}
		source := personaPath(argv[1], personas, globalPersonas)
		if source == "" {
			return fatal(fmt.Errorf("no SOUL.md found for persona %q", argv[1]))
		}
		_ = os.MkdirAll(personas, 0o700)
		_ = os.WriteFile(active, []byte(argv[1]+"\n"), 0o600)
		_ = os.Remove(filepath.Join(root, "SOUL.md"))
		relative, _ := filepath.Rel(root, source)
		if err := os.Symlink(relative, filepath.Join(root, "SOUL.md")); err != nil {
			raw, _ := os.ReadFile(source)
			_ = os.WriteFile(filepath.Join(root, "SOUL.md"), raw, 0o600)
		}
		return 0
	case "clear":
		_ = os.Remove(active)
		_ = os.Remove(filepath.Join(root, "SOUL.md"))
		_ = os.WriteFile(filepath.Join(root, "SOUL.md"), []byte("# No Active Persona\n\nNo orchestrator persona is active. Run `repowire orchestrator persona use <name>` to select one.\n"), 0o600)
		return 0
	default:
		return usage("orchestrator persona <list|show|path|use|clear>")
	}
}

func personaPath(name string, roots ...string) string {
	if !safeMemoryName(name) {
		return ""
	}
	for _, root := range roots {
		path := filepath.Join(root, name, "SOUL.md")
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

func readText(path string) string {
	raw, _ := os.ReadFile(path)
	return string(raw)
}
