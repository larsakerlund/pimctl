// Project-file tests cover discovery boundaries, strict parsing and exclusive
// creation. They use temporary repositories and never read a real Azure login.

package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	projectTenant = "11111111-1111-4111-8111-111111111111"
	projectRole   = "22222222-2222-4222-8222-222222222222"
	projectScope  = "/subscriptions/33333333-3333-4333-8333-333333333333/resourceGroups/dev"
)

func projectFixture() *Project {
	return &Project{
		Tenant: projectTenant,
		Roles:  []ProjectRole{{RoleDefinitionID: projectRole, Scope: projectScope, Name: "Reader"}},
	}
}

func TestProjectRoundTripAndNoOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), ProjectFile)
	if err := CreateProject(path, projectFixture()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // temporary file created by this test.
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "# Reader") || strings.Contains(string(data), "context") {
		t.Fatalf("unexpected file: %s", data)
	}
	p, err := LoadProject(path)
	if err != nil || p.Tenant != projectTenant || len(p.Roles) != 1 || p.Roles[0].Scope != projectScope {
		t.Fatalf("round trip: %#v, %v", p, err)
	}
	if createErr := CreateProject(path, projectFixture()); createErr == nil {
		t.Fatal("overwrote existing file")
	}
	after, err := os.ReadFile(path) //nolint:gosec // temporary file created by this test.
	if err != nil || !bytes.Equal(after, data) {
		t.Fatalf("existing file changed: %v", err)
	}
	link := filepath.Join(filepath.Dir(path), "link.yaml")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := CreateProject(link, projectFixture()); err == nil {
		t.Fatal("overwrote symlink")
	}
}

func TestLoadProjectRejectsBrokenRequirements(t *testing.T) {
	valid := "tenant: " + projectTenant + "\nroles:\n  - roleDefinitionId: " + projectRole + "\n    scope: " + projectScope + "\n"
	cases := map[string]string{
		"empty":          "",
		"unknown":        valid + "context: personal\n",
		"extra document": valid + "---\n{}\n",
		"bad tenant": strings.ReplaceAll(
			valid,
			projectTenant,
			"example.com",
		),
		"bad role": strings.ReplaceAll(valid, projectRole, "Reader"),
		"partial scope": strings.ReplaceAll(
			valid,
			projectScope,
			"/subscriptions/dev",
		),
		"duplicate key": valid + "tenant: " + projectTenant,
		"duplicate role": valid + "  - roleDefinitionId: " + projectRole + "\n    scope: " + strings.ToUpper(
			projectScope,
		) + "\n",
		"missing roles": "tenant: " + projectTenant + "\nroles: []\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ProjectFile)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadProject(path); err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("expected path-qualified failure: %v", err)
			}
		})
	}
}

func TestFindProjectStopsAtRepositoryBoundary(t *testing.T) {
	for _, worktree := range []bool{false, true} {
		t.Run(map[bool]string{false: "git directory", true: "worktree file"}[worktree], func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "repo")
			child := filepath.Join(root, "src", "component")
			if err := os.MkdirAll(child, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, ".git")
			if worktree {
				if err := os.WriteFile(marker, []byte("gitdir: somewhere"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(marker, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := CreateProject(filepath.Join(parent, ProjectFile), projectFixture()); err != nil {
				t.Fatal(err)
			}
			if got, err := FindProject(child); err != nil || got != "" {
				t.Fatalf("escaped repository: %q, %v", got, err)
			}
			want := filepath.Join(root, ProjectFile)
			if err := CreateProject(want, projectFixture()); err != nil {
				t.Fatal(err)
			}
			if got, err := FindProject(child); err != nil || got != want {
				t.Fatalf("discovery: %q, %v", got, err)
			}
			nearest := filepath.Join(filepath.Dir(child), ProjectFile)
			if err := CreateProject(nearest, projectFixture()); err != nil {
				t.Fatal(err)
			}
			if got, err := FindProject(child); err != nil || got != nearest {
				t.Fatalf("nearest: %q, %v", got, err)
			}
		})
	}
}

func TestValidateScope(t *testing.T) {
	for _, scope := range []string{projectScope, "/providers/Microsoft.Management/managementGroups/platform", projectScope + "/providers/Microsoft.Storage/storageAccounts/dev"} {
		if err := ValidateScope(scope); err != nil {
			t.Errorf("%s: %v", scope, err)
		}
	}
	for _, scope := range []string{projectScope + "/", projectScope + "/../prod", projectScope + "?x=y", projectScope + "/providers/Microsoft.Storage/storageAccounts", "https://management.azure.com" + projectScope} {
		if err := ValidateScope(scope); err == nil {
			t.Errorf("accepted %s", scope)
		}
	}
}
