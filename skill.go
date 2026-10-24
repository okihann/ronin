package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A skill is a directory containing SKILL.md. The file opens with YAML-ish
// frontmatter delimited by --- lines carrying at least name and description;
// everything after it is the instruction body, loaded only on demand.
//
// Progressive disclosure matters here: a chat model pays for every token it
// reads, so "skill list" prints one line per skill and "skill show" is what
// actually pulls the instructions into context.

type Skill struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Dir         string   `json:"dir"`
	Source      string   `json:"source"`
	Files       []string `json:"files,omitempty"`
	Body        string   `json:"-"`
}

type skillFront struct {
	name string
	desc string
	body string
}

// parseSkillFile splits frontmatter from body without pulling in a YAML
// dependency: the frontmatter is flat key: value pairs, which is all the
// format asks for.
func parseSkillFile(raw string) (skillFront, error) {
	var out skillFront
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	lines := strings.Split(raw, "\n")
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i >= len(lines) || strings.TrimSpace(lines[i]) != "---" {
		return out, fmt.Errorf("missing frontmatter: SKILL.md must start with a --- line")
	}
	i++
	start := i
	for i < len(lines) && strings.TrimSpace(lines[i]) != "---" {
		line := lines[i]
		if k, v, ok := strings.Cut(line, ":"); ok {
			key := strings.ToLower(strings.TrimSpace(k))
			val := strings.TrimSpace(v)
			val = strings.Trim(val, `"'`)
			switch key {
			case "name":
				out.name = val
			case "description":
				out.desc = val
			}
		}
		i++
	}
	if i >= len(lines) {
		return out, fmt.Errorf("missing closing --- for frontmatter (opened at line %d)", start)
	}
	out.body = strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
	if out.name == "" {
		return out, fmt.Errorf("frontmatter is missing a name")
	}
	if out.desc == "" {
		return out, fmt.Errorf("frontmatter is missing a description")
	}
	return out, nil
}

func skillRoots() []struct{ Dir, Source string } {
	var roots []struct{ Dir, Source string }
	if cfgDir, err := os.UserConfigDir(); err == nil && cfgDir != "" {
		roots = append(roots, struct{ Dir, Source string }{filepath.Join(cfgDir, "ronin", "skills"), "user"})
	}
	if wd, err := os.Getwd(); err == nil {
		roots = append(roots, struct{ Dir, Source string }{filepath.Join(wd, ".ronin", "skills"), "project"})
	}
	return roots
}

// discoverSkills walks each root one level deep. A directory without a
// readable SKILL.md is skipped rather than reported as an error, so a
// half-written skill does not break listing for everything else.
func discoverSkills() []Skill {
	byName := map[string]Skill{}
	var order []string
	for _, root := range skillRoots() {
		entries, err := os.ReadDir(root.Dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(root.Dir, e.Name())
			raw, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
			if err != nil {
				continue
			}
			front, err := parseSkillFile(string(raw))
			if err != nil {
				continue
			}
			s := Skill{
				Name:        front.name,
				Description: front.desc,
				Dir:         dir,
				Source:      root.Source,
				Body:        front.body,
				Files:       skillFiles(dir),
			}
			if _, seen := byName[s.Name]; !seen {
				order = append(order, s.Name)
			}
			byName[s.Name] = s // later roots win, so project overrides user
		}
	}
	sort.Strings(order)
	out := make([]Skill, 0, len(order))
	for _, n := range order {
		out = append(out, byName[n])
	}
	return out
}

func skillFiles(dir string) []string {
	var files []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "node_modules" || info.Name() == "__pycache__" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "SKILL.md" {
			return nil
		}
		if strings.HasPrefix(filepath.Base(rel), ".") {
			return nil
		}
		files = append(files, rel)
		if len(files) > 200 {
			return filepath.SkipAll
		}
		return nil
	})
	sort.Strings(files)
	return files
}

func findSkill(name string) (Skill, bool) {
	for _, s := range discoverSkills() {
		if s.Name == name {
			return s, true
		}
	}
	return Skill{}, false
}

func runSkill(args []string) int {
	asJSON := false
	var rest []string
	for _, a := range args {
		if a == "--json" {
			asJSON = true
			continue
		}
		rest = append(rest, a)
	}
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ronin skill list [--json]")
		fmt.Fprintln(os.Stderr, "       ronin skill show <name> [--json]")
		fmt.Fprintln(os.Stderr, "       ronin skill paths")
		return 2
	}
	switch rest[0] {
	case "list":
		return skillList(asJSON)
	case "show":
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "usage: ronin skill show <name>")
			return 2
		}
		return skillShow(rest[1], asJSON)
	case "paths":
		for _, r := range skillRoots() {
			fmt.Printf("%s (%s)\n", r.Dir, r.Source)
		}
		return 0
	default:
		fmt.Fprintln(os.Stderr, "unknown skill subcommand:", rest[0])
		return 2
	}
}

func skillList(asJSON bool) int {
	skills := discoverSkills()
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"count": len(skills), "skills": skills})
		return 0
	}
	if len(skills) == 0 {
		fmt.Println("no skills installed")
		for _, r := range skillRoots() {
			fmt.Printf("  searched: %s\n", r.Dir)
		}
		return 0
	}
	for _, s := range skills {
		fmt.Printf("%-24s [%s]  %s\n", s.Name, s.Source, s.Description)
		if len(s.Files) > 0 {
			fmt.Printf("%-24s %d bundled file(s): %s\n", "", len(s.Files), strings.Join(s.Files, ", "))
		}
	}
	fmt.Printf("\n%d skill(s). Run: ronin skill show <name>\n", len(skills))
	return 0
}

func skillShow(name string, asJSON bool) int {
	s, ok := findSkill(name)
	if !ok {
		fmt.Fprintf(os.Stderr, "no skill named %q\n", name)
		if avail := discoverSkills(); len(avail) > 0 {
			var names []string
			for _, a := range avail {
				names = append(names, a.Name)
			}
			fmt.Fprintf(os.Stderr, "available: %s\n", strings.Join(names, ", "))
		}
		return 1
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(s)
		return 0
	}
	fmt.Printf("# %s\n\n%s\n\n", s.Name, s.Description)
	fmt.Printf("location: %s (%s)\n", s.Dir, s.Source)
	if len(s.Files) > 0 {
		fmt.Printf("bundled files: %s\n", strings.Join(s.Files, ", "))
	}
	fmt.Println()
	fmt.Println(s.Body)
	return 0
}
