// Project requirements are a small, strict YAML document discovered locally.
// This file does not select accounts, resolve eligibility or call Azure.

package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"go.yaml.in/yaml/v3"
)

// ProjectFile is the only automatically discovered project filename.
const ProjectFile = ".pimctl.yaml"

// Project describes required access, independently of a developer's login or
// the scopes at which that developer was granted eligibility.
type Project struct {
	Tenant string        `json:"tenant" yaml:"tenant"` // Required Azure tenant UUID.
	Roles  []ProjectRole `json:"roles"  yaml:"roles"`  // Exact activation targets; never substring selectors.
}

// ProjectRole identifies the role and scope to activate, not its eligibility.
type ProjectRole struct {
	RoleDefinitionID string `json:"roleDefinitionId" yaml:"roleDefinitionId"` // Role definition UUID.
	Scope            string `json:"scope"            yaml:"scope"`            // Full ARM activation scope.
	Name             string `json:"-"                yaml:"-"`                // Generated comment for humans; not an identifier.
}

// FindProject searches start and its parents, stopping at the first Git root
// (directory or worktree marker), home, or filesystem root. It returns an empty
// path when absent; inaccessible files or boundaries are errors, not absence.
func FindProject(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	for {
		path := filepath.Join(dir, ProjectFile)
		if _, statErr := os.Lstat(path); statErr == nil {
			return path, nil
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		_, gitErr := os.Lstat(filepath.Join(dir, ".git"))
		if gitErr != nil && !errors.Is(gitErr, os.ErrNotExist) {
			return "", gitErr
		}
		parent := filepath.Dir(dir)
		if gitErr == nil || dir == home || parent == dir {
			return "", nil
		}
		dir = parent
	}
}

// LoadProject reads one explicitly selected file, rejecting unknown fields,
// extra documents and incomplete requirements. Errors include its path.
func LoadProject(path string) (*Project, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the caller explicitly selects a local configuration file.
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", path, err)
	}
	var p Project
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err = dec.Decode(&p); err == nil {
		var extra any
		if nextErr := dec.Decode(&extra); !errors.Is(nextErr, io.EOF) {
			err = errors.New("expected exactly one YAML document")
		}
	}
	if err == nil {
		err = p.Validate()
	}
	if err != nil {
		return nil, fmt.Errorf("invalid %s: %w", path, err)
	}
	return &p, nil
}

// Validate rejects missing or malformed identifiers and duplicate targets.
// It validates syntax only; Azure decides which eligibilities apply.
func (p *Project) Validate() error {
	if !validUUID(p.Tenant) {
		return errors.New("tenant must be a UUID")
	}
	if len(p.Roles) == 0 {
		return errors.New("roles must contain at least one role")
	}
	seen := map[string]bool{}
	for i, r := range p.Roles {
		if !validUUID(r.RoleDefinitionID) {
			return fmt.Errorf("roles[%d].roleDefinitionId must be a UUID", i)
		}
		if err := ValidateScope(r.Scope); err != nil {
			return fmt.Errorf("roles[%d].scope: %w", i, err)
		}
		key := strings.ToLower(r.Scope + "|" + r.RoleDefinitionID)
		if seen[key] {
			return fmt.Errorf("roles[%d] duplicates a role and scope", i)
		}
		seen[key] = true
	}
	return nil
}

// validUUID accepts only canonical UUID syntax, excluding nil identifiers.
func validUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && strings.EqualFold(value, id.String())
}

// ValidateScope accepts ARM management-group, subscription, resource-group and
// resource identifiers. URL query strings, path traversal and partial pairs
// are rejected before an identifier can become an HTTP request path.
func ValidateScope(scope string) error {
	parts := strings.Split(strings.TrimPrefix(scope, "/"), "/")
	if !strings.HasPrefix(scope, "/") || strings.ContainsAny(scope, "?#%\\\r\n\t ") {
		return errors.New("expected a full ARM scope ID without a query or fragment")
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return errors.New("invalid ARM scope path")
		}
	}
	if len(parts) == 4 && strings.EqualFold(parts[0], "providers") &&
		strings.EqualFold(parts[1], "Microsoft.Management") && strings.EqualFold(parts[2], "managementGroups") {
		return nil
	}
	if len(parts) < 2 || !strings.EqualFold(parts[0], "subscriptions") || !validUUID(parts[1]) {
		return errors.New("expected /subscriptions/<UUID> or /providers/Microsoft.Management/managementGroups/<name>")
	}
	return validateResourceScope(parts[2:])
}

// validateResourceScope checks pairs below a subscription without calling ARM.
func validateResourceScope(tail []string) error {
	if len(tail) >= 2 && strings.EqualFold(tail[0], "resourceGroups") {
		tail = tail[2:]
	}
	if len(tail) == 0 {
		return nil
	}
	if len(tail) < 4 || len(tail)%2 != 0 || !strings.EqualFold(tail[0], "providers") {
		return errors.New("expected a resource group or a complete provider/resource path")
	}
	return nil
}

// CreateProject writes a new file with readable role comments, refusing to
// replace any existing path. It returns validation or filesystem errors.
func CreateProject(path string, p *Project) error {
	if err := p.Validate(); err != nil {
		return err
	}
	var node yaml.Node
	if err := node.Encode(p); err != nil {
		return err
	}
	for i := 0; i < len(node.Content); i += 2 {
		if node.Content[i].Value != "roles" {
			continue
		}
		for j, role := range node.Content[i+1].Content {
			role.Content[1].LineComment = strings.Join(strings.Fields(p.Roles[j].Name), " ")
		}
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	const yamlIndent = 2
	enc.SetIndent(yamlIndent)
	if err := enc.Encode(&node); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return createProjectExclusive(path, b.Bytes())
}

// createProjectExclusive publishes a complete file without replacing any existing
// path. The temporary file is private and removed even when publication fails.
func createProjectExclusive(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".pimctl-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) //nolint:errcheck // best-effort temporary-file cleanup.
	_, writeErr := f.Write(data)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	if err := os.Link(f.Name(), path); err != nil {
		return fmt.Errorf("could not create %s: %w", path, err)
	}
	return nil
}
