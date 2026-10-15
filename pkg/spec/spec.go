// Package spec derives where an enforced artifact lands on a machine and
// validates the author-supplied names that derivation consumes. Every function
// is pure; the server, the status join and the agent all derive through here so
// a path has exactly one place to be correct.
package spec

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	v1 "github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1"
)

var (
	// ErrTypeNotEnforceable is returned for a type outside the enforceable set.
	ErrTypeNotEnforceable = errors.New("artifact type is not enforceable")
	// ErrScopeNotSupported is returned for any scope other than USER_GLOBAL or REPO_LOCAL.
	ErrScopeNotSupported = errors.New("scope is not supported for enforcement")
	// ErrIdeNotSupported is returned for an (ide, type, scope) with no known install
	// location; guessing one would place a file the status join never matches.
	ErrIdeNotSupported = errors.New("ide_type is not supported for this artifact type and scope")
	// ErrInvalidSlug is returned when a slug is off the path-safe charset.
	ErrInvalidSlug = errors.New("slug must match ^[a-z0-9][a-z0-9-]*$ and be at most 100 characters")
	// ErrInvalidRelativePath is returned for an empty, over-long, absolute,
	// traversing, or dash-led artifact-relative file name.
	ErrInvalidRelativePath = errors.New("file relative_path is not path-safe")
	// ErrInvalidHookEvent is returned when a hook event is not in the tool's vocabulary.
	ErrInvalidHookEvent = errors.New("hook event is not valid for this ide_type")
	// ErrMultiFileTypeNotAllowed is returned for a file set on any type but SKILL,
	// whatever its size: every other type carries its body in inline_content.
	ErrMultiFileTypeNotAllowed = errors.New("files is only supported for SKILL; every other type uses inline_content")
	// ErrPrimaryFileMissing is returned when a file set does not name its primary exactly once.
	ErrPrimaryFileMissing = errors.New("files must contain exactly one primary file (e.g. SKILL.md)")
	// ErrDuplicateRelativePath is returned when two files in a set share a relative_path.
	ErrDuplicateRelativePath = errors.New("duplicate file relative_path in files set")
)

// MaxSlugLen bounds a slug.
const MaxSlugLen = 100

// MaxRelativePathLen bounds an artifact-relative file name.
const MaxRelativePathLen = 255

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Each segment may start with a dot (.gitignore) but never a dash, so a
// flag-shaped basename like "-rf" cannot be placed on disk.
var relativePathPattern = regexp.MustCompile(`^[A-Za-z0-9.][A-Za-z0-9._-]*(/[A-Za-z0-9.][A-Za-z0-9._-]*)*$`)

// The instruction types are enforceable as-is; HOOK is enforceable but is code
// execution, so callers gate it separately. A type absent here is rejected, so
// a newly added enum value is never silently enforceable.
var instructionTypes = map[v1.ArtifactType]bool{
	v1.ArtifactType_ARTIFACT_TYPE_CLAUDE_MD: true,
	v1.ArtifactType_ARTIFACT_TYPE_AGENTS_MD: true,
	v1.ArtifactType_ARTIFACT_TYPE_SKILL:     true,
	v1.ArtifactType_ARTIFACT_TYPE_AGENT:     true,
	v1.ArtifactType_ARTIFACT_TYPE_RULE:      true,
}

var hookEvents = map[v1.IdeType]map[string]bool{
	v1.IdeType_IDE_TYPE_CLAUDE_CODE: {
		"PreToolUse": true, "PostToolUse": true, "SessionStart": true,
		"Notification": true, "PermissionRequest": true, "Stop": true,
		"UserPromptSubmit": true,
	},
	v1.IdeType_IDE_TYPE_CURSOR: {
		"sessionStart": true, "beforeSubmitPrompt": true, "beforeReadFile": true,
		"subagentStart": true, "stop": true,
	},
	v1.IdeType_IDE_TYPE_CODEX: {
		"SessionStart": true, "SubagentStart": true, "PreToolUse": true,
		"PostToolUse": true, "Stop": true,
	},
	v1.IdeType_IDE_TYPE_KIRO: {
		"SessionStart": true, "Stop": true, "PreToolUse": true, "PostToolUse": true,
		"PreTaskExec": true, "PostTaskExec": true, "UserPromptSubmit": true,
		"PostFileCreate": true, "PostFileSave": true, "PostFileDelete": true,
	},
}

// IsInstructionType reports whether t is an enforceable non-executable type.
func IsInstructionType(t v1.ArtifactType) bool {
	return instructionTypes[t]
}

// IsEnforceable reports whether t can be enforced at all (instruction types and HOOK).
func IsEnforceable(t v1.ArtifactType) bool {
	return IsInstructionType(t) || t == v1.ArtifactType_ARTIFACT_TYPE_HOOK
}

// IsRootDocType reports whether t derives one constant file per checkout (or home)
// regardless of slug, so two definitions of it at one target collide on disk.
func IsRootDocType(t v1.ArtifactType) bool {
	return t == v1.ArtifactType_ARTIFACT_TYPE_CLAUDE_MD || t == v1.ArtifactType_ARTIFACT_TYPE_AGENTS_MD
}

// ValidateSlug rejects a slug off the path-safe charset.
func ValidateSlug(slug string) error {
	if len(slug) > MaxSlugLen || !slugPattern.MatchString(slug) {
		return ErrInvalidSlug
	}
	return nil
}

// ValidateRelativePath rejects an artifact-relative file name that could escape
// the artifact directory; the charset alone admits a bare "..", hence the
// segment check.
func ValidateRelativePath(relPath string) error {
	if relPath == "" || len(relPath) > MaxRelativePathLen || !relativePathPattern.MatchString(relPath) {
		return ErrInvalidRelativePath
	}
	for _, seg := range strings.Split(relPath, "/") {
		if seg == "." || seg == ".." {
			return ErrInvalidRelativePath
		}
	}
	return nil
}

// ValidateHookEvent rejects an event outside the target tool's vocabulary, so a
// hook that could never fire is refused at the write.
func ValidateHookEvent(ide v1.IdeType, event string) error {
	events, ok := hookEvents[ide]
	if !ok {
		return ErrIdeNotSupported
	}
	if !events[event] {
		return ErrInvalidHookEvent
	}
	return nil
}

// InstallPath returns the primary file's path relative to the scope root (home
// for USER_GLOBAL, the checkout for REPO_LOCAL), from the slug and never the
// display name. The result is byte-identical to the source_path an agent
// reports for the same file, which is what makes the status join exact.
func InstallPath(ide v1.IdeType, t v1.ArtifactType, slug string, scope v1.ArtifactScope) (string, error) {
	switch scope {
	case v1.ArtifactScope_ARTIFACT_SCOPE_USER_GLOBAL, v1.ArtifactScope_ARTIFACT_SCOPE_REPO_LOCAL:
	default:
		return "", ErrScopeNotSupported
	}
	repoLocal := scope == v1.ArtifactScope_ARTIFACT_SCOPE_REPO_LOCAL

	switch t {
	case v1.ArtifactType_ARTIFACT_TYPE_HOOK:
		return hookScriptPath(ide, slug, repoLocal)
	case v1.ArtifactType_ARTIFACT_TYPE_CLAUDE_MD:
		// Root docs are tool-agnostic: placed regardless of which tool reads them.
		if repoLocal {
			return "CLAUDE.md", nil
		}
		return ".claude/CLAUDE.md", nil
	case v1.ArtifactType_ARTIFACT_TYPE_AGENTS_MD:
		if repoLocal {
			return "AGENTS.md", nil
		}
		// No user-scope AGENTS.md location is observed, so it would read PENDING forever.
		return "", ErrIdeNotSupported
	case v1.ArtifactType_ARTIFACT_TYPE_SKILL, v1.ArtifactType_ARTIFACT_TYPE_AGENT, v1.ArtifactType_ARTIFACT_TYPE_RULE:
		return instructionPath(ide, t, slug, repoLocal)
	default:
		return "", ErrTypeNotEnforceable
	}
}

func instructionPath(ide v1.IdeType, t v1.ArtifactType, slug string, repoLocal bool) (string, error) {
	var format string
	switch ide {
	case v1.IdeType_IDE_TYPE_CLAUDE_CODE:
		format = map[v1.ArtifactType]string{
			v1.ArtifactType_ARTIFACT_TYPE_SKILL: ".claude/skills/%s/SKILL.md",
			v1.ArtifactType_ARTIFACT_TYPE_AGENT: ".claude/agents/%s.md",
			v1.ArtifactType_ARTIFACT_TYPE_RULE:  ".claude/rules/%s.md",
		}[t]
	case v1.IdeType_IDE_TYPE_CURSOR:
		// .mdc, not .md: Cursor only loads .mdc as a rule.
		format = map[v1.ArtifactType]string{
			v1.ArtifactType_ARTIFACT_TYPE_SKILL: ".cursor/skills/%s/SKILL.md",
			v1.ArtifactType_ARTIFACT_TYPE_AGENT: ".cursor/agents/%s.md",
			v1.ArtifactType_ARTIFACT_TYPE_RULE:  ".cursor/rules/%s.mdc",
		}[t]
	case v1.IdeType_IDE_TYPE_CODEX:
		// Skills use the cross-tool .agents/skills tree, rooted at home (not the
		// Codex home) at user scope; rules and agents are observed only in repos.
		if repoLocal {
			format = map[v1.ArtifactType]string{
				v1.ArtifactType_ARTIFACT_TYPE_SKILL: ".agents/skills/%s/SKILL.md",
				v1.ArtifactType_ARTIFACT_TYPE_AGENT: ".codex/agents/%s.md",
				v1.ArtifactType_ARTIFACT_TYPE_RULE:  ".codex/rules/%s.md",
			}[t]
		} else if t == v1.ArtifactType_ARTIFACT_TYPE_SKILL {
			format = ".agents/skills/%s/SKILL.md"
		}
	case v1.IdeType_IDE_TYPE_KIRO:
		// Kiro uses one relative layout under ~/.kiro and <repo>/.kiro.
		format = map[v1.ArtifactType]string{
			v1.ArtifactType_ARTIFACT_TYPE_SKILL: ".kiro/skills/%s/SKILL.md",
			v1.ArtifactType_ARTIFACT_TYPE_AGENT: ".kiro/agents/%s.md",
			v1.ArtifactType_ARTIFACT_TYPE_RULE:  ".kiro/steering/%s.md",
		}[t]
	}
	if format == "" {
		return "", ErrIdeNotSupported
	}
	if err := ValidateSlug(slug); err != nil {
		return "", fmt.Errorf("validating slug: %w", err)
	}
	return fmt.Sprintf(format, slug), nil
}

func hookScriptPath(ide v1.IdeType, slug string, repoLocal bool) (string, error) {
	if err := ValidateSlug(slug); err != nil {
		return "", fmt.Errorf("validating slug: %w", err)
	}
	if ide == v1.IdeType_IDE_TYPE_KIRO {
		// A Kiro hook is a self-contained, workspace-only manifest.
		if !repoLocal {
			return "", ErrIdeNotSupported
		}
		return fmt.Sprintf(".kiro/hooks/%s.kiro.hook", slug), nil
	}
	dir, ok := hookConfigDir(ide)
	if !ok {
		return "", ErrIdeNotSupported
	}
	// A user-scope Codex path is relative to the Codex home, which already is .codex.
	if ide == v1.IdeType_IDE_TYPE_CODEX && !repoLocal {
		return fmt.Sprintf("hooks/%s.sh", slug), nil
	}
	return fmt.Sprintf("%s/hooks/%s.sh", dir, slug), nil
}

func hookConfigDir(ide v1.IdeType) (string, bool) {
	switch ide {
	case v1.IdeType_IDE_TYPE_CLAUDE_CODE:
		return ".claude", true
	case v1.IdeType_IDE_TYPE_CURSOR:
		return ".cursor", true
	case v1.IdeType_IDE_TYPE_CODEX:
		return ".codex", true
	default:
		return "", false
	}
}

// HookCommand returns the command a hook's settings entry runs: always the
// artifact's own placed script, so what executes is covered by the file hash.
// An empty command with a nil error means the hook file is self-activating
// (Kiro) and needs no settings entry.
func HookCommand(ide v1.IdeType, scope v1.ArtifactScope, slug string) (string, error) {
	if ide == v1.IdeType_IDE_TYPE_KIRO {
		return "", nil
	}
	dir, ok := hookConfigDir(ide)
	if !ok {
		return "", ErrIdeNotSupported
	}
	if err := ValidateSlug(slug); err != nil {
		return "", fmt.Errorf("validating slug: %w", err)
	}
	switch scope {
	case v1.ArtifactScope_ARTIFACT_SCOPE_USER_GLOBAL:
		// The Codex script is placed under the Codex home, so the command must
		// resolve CODEX_HOME the same way the placement does.
		if ide == v1.IdeType_IDE_TYPE_CODEX {
			return fmt.Sprintf(`"${CODEX_HOME:-$HOME/.codex}"/hooks/%s.sh`, slug), nil
		}
		return fmt.Sprintf(`"$HOME"/%s/hooks/%s.sh`, dir, slug), nil
	case v1.ArtifactScope_ARTIFACT_SCOPE_REPO_LOCAL:
		switch ide {
		case v1.IdeType_IDE_TYPE_CLAUDE_CODE:
			return fmt.Sprintf(`"$CLAUDE_PROJECT_DIR"/%s/hooks/%s.sh`, dir, slug), nil
		case v1.IdeType_IDE_TYPE_CURSOR:
			return fmt.Sprintf(`%s/hooks/%s.sh`, dir, slug), nil
		default:
			return fmt.Sprintf(`"$(git rev-parse --show-toplevel)"/%s/hooks/%s.sh`, dir, slug), nil
		}
	default:
		return "", ErrScopeNotSupported
	}
}

// FileInstallPath places one file of an artifact: the primary (relPath "")
// lands exactly at root, a helper beside it in the artifact's directory.
// relPath must already have passed ValidateRelativePath.
func FileInstallPath(root, relPath string) string {
	if relPath == "" {
		return root
	}
	return path.Join(path.Dir(root), relPath)
}

// PrimaryFileName is the name the primary must carry in an authored file set
// (a skill's "SKILL.md").
func PrimaryFileName(root string) string {
	return path.Base(root)
}

// ValidateFileSet checks an authored file set against its derived root and
// returns the index of the primary. Only SKILL owns a directory; every other
// type is one file in a shared config dir where a helper would clobber config.
func ValidateFileSet(t v1.ArtifactType, root string, relPaths []string) (int, error) {
	if t != v1.ArtifactType_ARTIFACT_TYPE_SKILL {
		return -1, ErrMultiFileTypeNotAllowed
	}
	want := PrimaryFileName(root)
	seen := make(map[string]bool, len(relPaths))
	primary := -1
	for i, rel := range relPaths {
		if err := ValidateRelativePath(rel); err != nil {
			return -1, fmt.Errorf("validating file %q: %w", rel, err)
		}
		if seen[rel] {
			return -1, ErrDuplicateRelativePath
		}
		seen[rel] = true
		if rel == want {
			primary = i
		}
	}
	if primary < 0 {
		return -1, ErrPrimaryFileMissing
	}
	return primary, nil
}

// PreviewFile is one scope-root-relative path an artifact would place.
type PreviewFile struct {
	RelativePath string
	IsPrimary    bool
}

// Preview derives the install path(s) at scope of an artifact that need not
// exist: the single primary when relPaths is empty, else each file in order.
func Preview(ide v1.IdeType, t v1.ArtifactType, slug string, scope v1.ArtifactScope, relPaths []string) ([]PreviewFile, error) {
	root, err := InstallPath(ide, t, slug, scope)
	if err != nil {
		return nil, fmt.Errorf("deriving install path: %w", err)
	}
	if len(relPaths) == 0 {
		return []PreviewFile{{RelativePath: root, IsPrimary: true}}, nil
	}
	primary, err := ValidateFileSet(t, root, relPaths)
	if err != nil {
		return nil, fmt.Errorf("validating file set: %w", err)
	}
	out := make([]PreviewFile, 0, len(relPaths))
	for i, rel := range relPaths {
		out = append(out, PreviewFile{RelativePath: FileInstallPath(root, rel), IsPrimary: i == primary})
	}
	return out, nil
}
