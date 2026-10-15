// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package settings parses a Claude Code settings.json body into the synthetic harness
// artifacts (per-hook HOOK rows and a single PERMISSIONS row) the DETECTED detectors
// evaluate. It is a pure parser: it imports only the common proto enums and the standard
// library, with no DB or tenancy dependency.
package settings

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
)

// MaxSettingsChildren is the per-settings.json cap on derived children: a crafted settings file
// cannot force an unbounded fan-out. The parser stops emitting once the deduped, sorted child
// list reaches this length and reports capped=true so the caller can log the drop.
const MaxSettingsChildren = 500

// SettingsCarrierPath is the repo-relative path of the Claude Code settings file whose body is
// fanned out into synthetic per-hook and permissions children. It is the ONLY shared-manifest
// hook config ingested this way — other shared-manifest hook configs (.codex/hooks.json,
// .cursor/hooks.json, …) stay excluded because no per-hook identity contract exists for them.
const SettingsCarrierPath = ".claude/settings.json"

// settingsChildHookPrefix / settingsChildPermissions are the synthetic source_path suffixes that
// keep each derived child distinct from the carrier and from each other on the
// (ide_type, repository, source_path) identity key. The bare carrier path is never persisted:
// only suffixed children reach the artifact inventory.
const (
	settingsChildHookPrefix  = SettingsCarrierPath + "#hook:"
	settingsChildPermissions = SettingsCarrierPath + "#permissions"
	// settingsChildEmptyMarker records a settings.json that parsed to no real artifacts (no hooks,
	// empty/absent permissions) or a malformed body, so the carrier converges on its blob instead
	// of re-fetching every scan. It carries type UNSPECIFIED — a convergence-only marker, not a
	// derived artifact.
	settingsChildEmptyMarker = SettingsCarrierPath + "#empty"
)

// SettingsEmptySourcePath is the convergence sentinel the caller upserts when a settings.json
// parses to no real children (or was truncated at the fetch cap), so the carrier converges on the
// blob rather than re-fetching every scan. It carries the settings-child "#" suffix but no derived
// artifact.
const SettingsEmptySourcePath = settingsChildEmptyMarker

// SettingsChildPathPrefix is the source_path prefix every settings.json child shares (the carrier
// path + "#").
const SettingsChildPathPrefix = SettingsCarrierPath + "#"

// IsSettingsCarrier reports whether (ideType, sourcePath) is the physical Claude Code settings
// file — the file read only to fan out its body, never persisted as its own row.
func IsSettingsCarrier(ideType commonv1.IdeType, sourcePath string) bool {
	return ideType == commonv1.IdeType_IDE_TYPE_CLAUDE_CODE && sourcePath == SettingsCarrierPath
}

// SettingsChild is one synthetic artifact fanned out from a settings.json body: a per-hook HOOK or
// the single repo PERMISSIONS artifact. SourcePath is the suffixed identity key; the children all
// share the carrier body as their primary file.
type SettingsChild struct {
	SourcePath   string
	ArtifactType commonv1.HarnessArtifactType
	Name         string
}

// settingsFile is the local parse shape of a Claude Code settings.json. Permissions is a
// map[string]any because only its presence/non-emptiness is read here.
type settingsFile struct {
	Hooks       map[string][]settingsHookGroup `json:"hooks"`
	Permissions map[string]any                 `json:"permissions"`
}

type settingsHookGroup struct {
	Matcher string            `json:"matcher"`
	Hooks   []settingsHookCmd `json:"hooks"`
}

type settingsHookCmd struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// ParseSettingsArtifacts fans a Claude Code settings.json body out into its synthetic children:
// one HOOK per (event, matcher, command) hook entry with a non-empty command, plus one PERMISSIONS
// artifact when the body carries a non-empty permissions block. A malformed body yields
// (nil, false, err): the err names the parse failure so the caller can surface it, but a bad
// committed settings file must not FAIL the scan — the caller logs it and derives no artifacts.
//
// Each HOOK's source_path suffix is a length-prefixed sha256 of (event, matcher, command), so two
// identical hook tuples collapse to one child and distinct tuples never collide on the identity key
// even when a delimiter byte appears inside a value. The returned children carry distinct
// source_paths in a deterministic order. capped is true when the uncapped child total would exceed
// MaxSettingsChildren; the returned list is then truncated to that many children, still in
// deterministic order.
func ParseSettingsArtifacts(body []byte) (children []SettingsChild, capped bool, err error) {
	var settings settingsFile
	if unmarshalErr := json.Unmarshal(body, &settings); unmarshalErr != nil {
		return nil, false, fmt.Errorf("unmarshalling settings.json body: %w", unmarshalErr)
	}

	seen := make(map[string]struct{})

	eventNames := make([]string, 0, len(settings.Hooks))
	for eventName := range settings.Hooks {
		eventNames = append(eventNames, eventName)
	}
	sort.Strings(eventNames)

	for _, eventName := range eventNames {
		for _, group := range settings.Hooks[eventName] {
			for _, hookCmd := range group.Hooks {
				if hookCmd.Command == "" {
					continue
				}
				sourcePath := settingsChildHookPrefix + hookIdentityDigest(eventName, group.Matcher, hookCmd.Command)
				if _, dup := seen[sourcePath]; dup {
					continue
				}
				seen[sourcePath] = struct{}{}
				name := eventName
				if group.Matcher != "" {
					name = eventName + ":" + group.Matcher
				}
				children = append(children, SettingsChild{
					SourcePath:   sourcePath,
					ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_HOOK,
					Name:         name,
				})
			}
		}
	}

	hasPermissions := len(settings.Permissions) > 0

	// Cap the HOOK fan-out AFTER the deterministic dedup/sort, reserving one slot for the
	// permissions child when present. Two guarantees: (a) a crafted settings file cannot force an
	// unbounded fan-out; (b) a file with many hooks cannot push the permissions child out of the
	// capped set — it is appended last, so a uniform total-cap would slice it off and the
	// permission-inventory detector would never see this file's permissions. children currently
	// holds only hooks, so cap here, then append permissions into the reserved slot.
	hookBudget := MaxSettingsChildren
	if hasPermissions {
		hookBudget = MaxSettingsChildren - 1
	}
	if len(children) > hookBudget {
		children = children[:hookBudget]
		capped = true
	}

	if hasPermissions {
		children = append(children, SettingsChild{
			SourcePath:   settingsChildPermissions,
			ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_PERMISSIONS,
			Name:         "permissions",
		})
	}

	return children, capped, nil
}

// hookIdentityDigest returns the length-prefixed sha256 hex of (event, matcher, command). Each part
// is prefixed with its 8-byte big-endian length before its bytes, so the encoding is unambiguous: a
// delimiter byte inside one value can never be confused for the boundary between values, so distinct
// triples never collide.
func hookIdentityDigest(event, matcher, command string) string {
	hasher := sha256.New()
	for _, part := range []string{event, matcher, command} {
		var lenPrefix [8]byte
		binary.BigEndian.PutUint64(lenPrefix[:], uint64(len(part)))
		hasher.Write(lenPrefix[:])
		hasher.Write([]byte(part))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}
