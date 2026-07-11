package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func seedStaging(t *testing.T, root string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "build", "cli-docs", "index.md"), `---
title: CLI Reference
description: Generated reference for every bd command
---

<!-- AUTO-GENERATED: do not edit manually -->

Generated from `+"`bd help --docs-root`"+`.

## Commands

- [`+"`bd create`"+`](./create.md)
- [`+"`bd show`"+`](./show.md)
`)
	writeFile(t, filepath.Join(root, "build", "cli-docs", "show.md"), `---
title: "bd show"
description: "Show an issue"
---

<!-- AUTO-GENERATED: do not edit manually -->

Generated from `+"`bd help --doc show`"+`.

Show an issue

`+"```"+`
bd show <id>
`+"```"+`
`)
	writeFile(t, filepath.Join(root, "build", "cli-docs", "create.md"), `---
title: "bd create"
---

<!-- AUTO-GENERATED: do not edit manually -->

Create an issue
`)
	writeFile(t, filepath.Join(root, "docs", "docs.json"), `{
  "name": "Beads Documentation",
  "navigation": {
    "groups": [
      {
        "group": "Getting Started",
        "pages": [
          "index"
        ]
      },
      {
        "group": "CLI Reference",
        "pages": [
          "cli-reference/index",
          "cli-reference/stale-command"
        ]
      }
    ]
  }
}
`)
}

func TestRunTransformsPagesToMintlifyForm(t *testing.T) {
	root := t.TempDir()
	seedStaging(t, root)

	if err := run(root); err != nil {
		t.Fatalf("run() error = %v", err)
	}

	show, err := os.ReadFile(filepath.Join(root, "docs", "cli-reference", "show.md"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(show)
	if !strings.Contains(got, "{/* AUTO-GENERATED: do not edit manually */}") {
		t.Errorf("show.md missing JSX comment marker:\n%s", got)
	}
	if strings.Contains(got, "<!--") {
		t.Errorf("show.md still contains an HTML comment (invalid in Mintlify MDX):\n%s", got)
	}
	if !strings.Contains(got, "bd show <id>") {
		t.Errorf("show.md lost its usage fence:\n%s", got)
	}

	index, err := os.ReadFile(filepath.Join(root, "docs", "cli-reference", "index.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), "](/cli-reference/show)") {
		t.Errorf("index.md links not rewritten to extensionless routes:\n%s", index)
	}
	if strings.Contains(string(index), "](./") {
		t.Errorf("index.md still contains relative .md links:\n%s", index)
	}
}

func TestRunSplicesNavAndPreservesOtherGroups(t *testing.T) {
	root := t.TempDir()
	seedStaging(t, root)

	if err := run(root); err != nil {
		t.Fatalf("run() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(root, "docs", "docs.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)

	for _, want := range []string{
		`"cli-reference/index"`,
		`"cli-reference/create"`,
		`"cli-reference/show"`,
		`"Getting Started"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("docs.json missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "stale-command") {
		t.Errorf("docs.json still lists a stale CLI page:\n%s", got)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("docs.json is no longer valid JSON: %v\n%s", err, got)
	}

	// Idempotent: a second run must not change anything.
	if err := run(root); err != nil {
		t.Fatalf("run() second pass error = %v", err)
	}
	again, err := os.ReadFile(filepath.Join(root, "docs", "docs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, again) {
		t.Errorf("nav splice is not idempotent:\nfirst:\n%s\nsecond:\n%s", data, again)
	}
}

func TestRunRemovesStaleTargetPages(t *testing.T) {
	root := t.TempDir()
	seedStaging(t, root)
	stale := filepath.Join(root, "docs", "cli-reference", "removed-command.md")
	writeFile(t, stale, "stale\n")

	if err := run(root); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale target page survived the run")
	}
}

func TestRunFailsLoudlyWithoutStaging(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "docs", "docs.json"), `{"navigation":{"groups":[{"group":"CLI Reference","pages":[]}]}}`)

	err := run(root)
	if err == nil {
		t.Fatal("run() succeeded with no staging tree; want loud failure")
	}
	if !strings.Contains(err.Error(), "build/cli-docs") {
		t.Errorf("error should name the missing staging dir: %v", err)
	}
}

func TestRunFailsLoudlyWithoutCLIReferenceGroup(t *testing.T) {
	root := t.TempDir()
	seedStaging(t, root)
	writeFile(t, filepath.Join(root, "docs", "docs.json"), `{"navigation":{"groups":[{"group":"Other","pages":[]}]}}`)

	err := run(root)
	if err == nil {
		t.Fatal("run() succeeded without a CLI Reference nav group; want loud failure")
	}
	if !strings.Contains(err.Error(), "CLI Reference") {
		t.Errorf("error should name the missing group: %v", err)
	}
}
