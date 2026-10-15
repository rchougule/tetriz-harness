package spec

import (
	"errors"
	"path"

	v1 "github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1"
)

// ErrRootUnknown is returned when the root a placement needs was not supplied.
var ErrRootUnknown = errors.New("scope root is not known")

// Roots are the machine-specific directories scope-root-relative paths resolve
// against.
type Roots struct {
	// Home is the user's home directory.
	Home string
	// CodexHome is the Codex config root; empty means Home/.codex.
	CodexHome string
	// RepoRoot is the checkout a REPO_LOCAL artifact is placed into.
	RepoRoot string
}

// InstallRoot returns the directory an InstallPath result is relative to.
// REPO_LOCAL resolves against the checkout. USER_GLOBAL resolves against home,
// except a Codex hook script, whose path is relative to the Codex home; a
// Codex skill and CLAUDE.md stay home-relative.
func InstallRoot(ide v1.IdeType, t v1.ArtifactType, scope v1.ArtifactScope, roots Roots) (string, error) {
	switch scope {
	case v1.ArtifactScope_ARTIFACT_SCOPE_REPO_LOCAL:
		if roots.RepoRoot == "" {
			return "", ErrRootUnknown
		}
		return roots.RepoRoot, nil
	case v1.ArtifactScope_ARTIFACT_SCOPE_USER_GLOBAL:
		if roots.Home == "" {
			return "", ErrRootUnknown
		}
		if ide == v1.IdeType_IDE_TYPE_CODEX && t == v1.ArtifactType_ARTIFACT_TYPE_HOOK {
			if roots.CodexHome != "" {
				return roots.CodexHome, nil
			}
			return path.Join(roots.Home, ".codex"), nil
		}
		return roots.Home, nil
	default:
		return "", ErrScopeNotSupported
	}
}
