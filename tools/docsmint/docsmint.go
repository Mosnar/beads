package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	// Full HTML comments on one line. bd's generic pages use these as
	// auto-generated markers; Mintlify parses .md through MDX, where HTML
	// comments are invalid, so they become JSX comments.
	htmlCommentRE = regexp.MustCompile(`<!--\s*(.*?)\s*-->`)
	// Relative page links in the generic index (./show.md). The deployed
	// Mintlify site serves extensionless routes.
	relativePageLinkRE = regexp.MustCompile(`\]\(\./([A-Za-z0-9_-]+)\.md\)`)
)

// run post-processes the generic staging tree at <root>/build/cli-docs into
// the committed Mintlify pages at <root>/docs/cli-reference and splices the
// CLI Reference pages array in <root>/docs/docs.json.
func run(root string) error {
	staging := filepath.Join(root, "build", "cli-docs")
	target := filepath.Join(root, "docs", "cli-reference")
	docsJSON := filepath.Join(root, "docs", "docs.json")

	entries, err := os.ReadDir(staging)
	if err != nil {
		return fmt.Errorf("reading staging tree %s (run `bd help --docs-root` first): %w", staging, err)
	}

	var pages []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		pages = append(pages, entry.Name())
	}
	if len(pages) == 0 {
		return fmt.Errorf("staging tree %s contains no markdown pages (run `bd help --docs-root` first)", staging)
	}
	sort.Strings(pages)

	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	if err := removeMarkdownFiles(target); err != nil {
		return err
	}

	var navPages []string
	for _, name := range pages {
		// #nosec G304: name comes from os.ReadDir over the repo's own staging
		// tree (filtered to *.md above), not from external input.
		data, err := os.ReadFile(filepath.Join(staging, name))
		if err != nil {
			return err
		}
		out := transformPage(string(data))
		// #nosec G306: generated repository Markdown should be readable like source files.
		if err := os.WriteFile(filepath.Join(target, name), []byte(out), 0o644); err != nil {
			return err
		}
		if name != "index.md" {
			navPages = append(navPages, "cli-reference/"+strings.TrimSuffix(name, ".md"))
		}
	}

	return spliceCLINav(docsJSON, append([]string{"cli-reference/index"}, navPages...))
}

// transformPage converts one generic page to Mintlify form.
func transformPage(content string) string {
	content = htmlCommentRE.ReplaceAllString(content, "{/* $1 */}")
	content = relativePageLinkRE.ReplaceAllString(content, "](/cli-reference/$1)")
	content = neutralizeESMHazards(content)
	return content
}

// neutralizeESMHazards wraps prose lines whose first token is `export` or
// `import` in inline code spans. Such lines are legal CommonMark in bd's
// generic output (indented example lines in help text), but Mintlify's
// prebuild dedents them to column 0 and then parses them as MDX ESM blocks,
// which fails the entire page (observed on cli-reference/mail). Lines inside
// code fences are left untouched.
func neutralizeESMHazards(content string) string {
	lines := strings.Split(content, "\n")
	inFence := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if strings.HasPrefix(trimmed, "export ") || strings.HasPrefix(trimmed, "import ") {
			indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			delim := "`"
			if strings.Contains(trimmed, "`") {
				delim = "``"
			}
			lines[i] = indent + delim + trimmed + delim
		}
	}
	return strings.Join(lines, "\n")
}

func removeMarkdownFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// spliceCLINav rewrites the "pages" array of the "CLI Reference" navigation
// group in docs.json, leaving every other byte of the file untouched. A
// textual splice (rather than a JSON round-trip) preserves key order and
// formatting so regeneration is diff-stable. The pages entries are plain
// slugs, so bracket matching cannot be confused by string contents.
func spliceCLINav(docsJSONPath string, pages []string) error {
	// #nosec G304: docsJSONPath is derived from the repo root argument this
	// developer tool is invoked with; it only ever reads the repo's docs.json.
	data, err := os.ReadFile(docsJSONPath)
	if err != nil {
		return err
	}
	s := string(data)

	groupIdx := strings.Index(s, `"group": "CLI Reference"`)
	if groupIdx < 0 {
		return fmt.Errorf("%s: no \"CLI Reference\" navigation group found", docsJSONPath)
	}
	pagesIdx := strings.Index(s[groupIdx:], `"pages"`)
	if pagesIdx < 0 {
		return fmt.Errorf("%s: \"CLI Reference\" group has no \"pages\" key", docsJSONPath)
	}
	pagesIdx += groupIdx
	openIdx := strings.Index(s[pagesIdx:], "[")
	if openIdx < 0 {
		return fmt.Errorf("%s: \"CLI Reference\" pages key has no array", docsJSONPath)
	}
	openIdx += pagesIdx

	depth := 0
	closeIdx := -1
	for i := openIdx; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				closeIdx = i
			}
		}
		if closeIdx >= 0 {
			break
		}
	}
	if closeIdx < 0 {
		return fmt.Errorf("%s: unbalanced \"pages\" array in \"CLI Reference\" group", docsJSONPath)
	}

	lineStart := strings.LastIndex(s[:pagesIdx], "\n") + 1
	indent := s[lineStart:pagesIdx]
	entryIndent := indent + "  "

	var b strings.Builder
	b.WriteString("[\n")
	for i, p := range pages {
		// #nosec G705: not a web-output sink — this developer tool writes the
		// repo's own docs.json; entries are %q-quoted slugs derived from the
		// staging tree's filenames, which the same pipeline generates.
		fmt.Fprintf(&b, "%s%q", entryIndent, p)
		if i < len(pages)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString(indent + "]")

	out := s[:openIdx] + b.String() + s[closeIdx+1:]
	// #nosec G306: docs.json is repository source, readable like other source files.
	return os.WriteFile(docsJSONPath, []byte(out), 0o644)
}
