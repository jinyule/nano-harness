package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validDescription = "Use when running a focused repository validation workflow."

func TestValidateSkills_ValidSkill(t *testing.T) {
	repository := t.TempDir()
	root := filepath.Join(repository, ".agents", "skills")
	writeSkill(t, root, "valid-skill", validDescription, "# Valid skill\n\nRead [the root rules](../../../AGENTS.md).\n", validOpenAI("valid-skill"))
	writeFile(t, filepath.Join(repository, "AGENTS.md"), "# Rules\n")

	count, err := validateSkills(root)
	if err != nil {
		t.Fatalf("validateSkills() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("validateSkills() count = %d, want 1", count)
	}
}

func TestValidateSkills_RejectsMissingOrEmptyRoot(t *testing.T) {
	if _, err := validateSkills(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("validateSkills() accepted a missing root")
	}

	empty := t.TempDir()
	if _, err := validateSkills(empty); err == nil || !strings.Contains(err.Error(), "no skill") {
		t.Fatalf("validateSkills() empty error = %v", err)
	}
}

func TestValidateSkill_RejectsInvalidContracts(t *testing.T) {
	tests := map[string]struct {
		directory   string
		frontmatter string
		body        string
		openAI      string
		want        string
	}{
		"unexpected frontmatter": {
			frontmatter: "name: invalid-skill\ndescription: " + validDescription + "\nlicense: MIT",
			body:        "# Skill\n",
			openAI:      validOpenAI("invalid-skill"),
			want:        "only name and description",
		},
		"missing description": {
			frontmatter: "name: invalid-skill\nmetadata: value",
			body:        "# Skill\n",
			openAI:      validOpenAI("invalid-skill"),
			want:        "requires name and description",
		},
		"bad name": {
			frontmatter: "name: Invalid_Skill\ndescription: " + validDescription,
			body:        "# Skill\n",
			openAI:      validOpenAI("Invalid_Skill"),
			want:        "invalid skill name",
		},
		"directory mismatch": {
			directory:   "different-name",
			frontmatter: "name: invalid-skill\ndescription: " + validDescription,
			body:        "# Skill\n",
			openAI:      validOpenAI("invalid-skill"),
			want:        "does not match directory",
		},
		"empty description": {
			frontmatter: "name: invalid-skill\ndescription: \"\"",
			body:        "# Skill\n",
			openAI:      validOpenAI("invalid-skill"),
			want:        "1 through 1024",
		},
		"angle bracket": {
			frontmatter: "name: invalid-skill\ndescription: Use <scope> when validating a repository workflow.",
			body:        "# Skill\n",
			openAI:      validOpenAI("invalid-skill"),
			want:        "angle brackets",
		},
		"todo placeholder": {
			frontmatter: "name: invalid-skill\ndescription: " + validDescription,
			body:        "# Skill\n\n[TODO: finish this]\n",
			openAI:      validOpenAI("invalid-skill"),
			want:        "unfinished TODO",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			repository := t.TempDir()
			directoryName := test.directory
			if directoryName == "" {
				directoryName = "invalid-skill"
			}
			directory := filepath.Join(repository, ".agents", "skills", directoryName)
			writeRawSkill(t, directory, test.frontmatter, test.body, test.openAI)
			err := validateSkill(directory, repository)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateSkill() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestParseFrontmatter_RejectsMalformedInput(t *testing.T) {
	tests := []string{
		"name: skill\n",
		"---\nname: skill\n",
		"---\ninvalid\n---\nbody",
		"---\nname: skill\nname: other\n---\nbody",
		"---\nname: \"unterminated\n---\nbody",
	}
	for _, input := range tests {
		if _, _, err := parseFrontmatter(input); err == nil {
			t.Fatalf("parseFrontmatter(%q) unexpectedly succeeded", input)
		}
	}
}

func TestValidateOpenAI_RejectsInvalidMetadata(t *testing.T) {
	tests := map[string]struct {
		content string
		want    string
	}{
		"shape":   {content: "interface:\n", want: "three supported"},
		"display": {content: "interface:\n  wrong: \"Name\"\n  short_description: \"Run focused repository checks\"\n  default_prompt: \"Use $valid-skill.\"\n", want: "display_name"},
		"short":   {content: "interface:\n  display_name: \"Name\"\n  short_description: \"Too short\"\n  default_prompt: \"Use $valid-skill.\"\n", want: "25 through 64"},
		"prompt":  {content: "interface:\n  display_name: \"Name\"\n  short_description: \"Run focused repository checks\"\n  default_prompt: \"Use another skill.\"\n", want: "$valid-skill"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "openai.yaml")
			writeFile(t, path, test.content)
			err := validateOpenAI(path, "valid-skill")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateOpenAI() error = %v, want substring %q", err, test.want)
			}
		})
	}

	if err := validateOpenAI(filepath.Join(t.TempDir(), "missing"), "valid-skill"); err == nil {
		t.Fatal("validateOpenAI() accepted a missing file")
	}
}

func TestValidateMarkdownLinks_RejectsBrokenAndEscapingLinks(t *testing.T) {
	for name, target := range map[string]string{
		"broken":   "missing.md",
		"escaping": "../../../../outside.md",
		"bad url":  "%zz.md",
	} {
		t.Run(name, func(t *testing.T) {
			repository := t.TempDir()
			directory := filepath.Join(repository, ".agents", "skills", "valid-skill")
			writeFile(t, filepath.Join(directory, "SKILL.md"), "# Skill\n\n[bad]("+target+")\n")
			if err := validateMarkdownLinks(directory, repository); err == nil {
				t.Fatalf("validateMarkdownLinks() accepted %q", target)
			}
		})
	}
}

func TestSkipLink(t *testing.T) {
	for _, target := range []string{"", "#part", "https://example.com", "http://example.com", "mailto:test@example.com"} {
		if !skipLink(target) {
			t.Fatalf("skipLink(%q) = false", target)
		}
	}
	if skipLink("relative.md") {
		t.Fatal("skipLink(relative.md) = true")
	}
}

func validOpenAI(name string) string {
	return "interface:\n" +
		"  display_name: \"Valid Skill\"\n" +
		"  short_description: \"Run focused repository checks\"\n" +
		"  default_prompt: \"Use $" + name + " for this task.\"\n"
}

func writeSkill(t *testing.T, root, name, description, body, openAI string) {
	t.Helper()
	writeRawSkill(t, filepath.Join(root, name), "name: "+name+"\ndescription: "+description, body, openAI)
}

func writeRawSkill(t *testing.T, directory, frontmatter, body, openAI string) {
	t.Helper()
	writeFile(t, filepath.Join(directory, "SKILL.md"), "---\n"+frontmatter+"\n---\n\n"+body)
	writeFile(t, filepath.Join(directory, "agents", "openai.yaml"), openAI)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}
