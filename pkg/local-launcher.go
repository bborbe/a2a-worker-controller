// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bborbe/errors"
	vaultconfig "github.com/bborbe/vault-cli/pkg/config"
	"gopkg.in/yaml.v3"
)

// The local backend's launcher and role resolution: which script a worker's tab execs, and which
// window and chip colour that tab gets.
//
// Both rules exist because a task carries its own answer and this backend used to ignore it.
// `launcher:` is honoured by the supervisor's `spawn_agent` (claude-supervisor
// `docs/fleet-surface.md` § Spawn a worker, item 8) but not here, so `/supervisor:open` — whose
// create branch routes an absent `target:` to this backend — opened a worker on the vault default
// while the same task opened through `spawn_agent` got its own launcher. `role:` decides the window
// and the chip colour, which is what makes a manager's tab distinguishable from a worker's.
const (
	// DefaultLocalGoalsDir is the vault directory a goal file lives in, relative to the configured
	// vault root — the sibling of DefaultLocalTasksDir.
	//
	// ⚠️ IT IS ONLY A FALLBACK, AND THAT IS THE DIFFERENCE FROM DefaultLocalTasksDir. A task's goal
	// links are resolved to FILE PATHS, and the goals directory is a vault-cli setting
	// (`goals_dir`) rather than a vault invariant: private-personal uses `24 Goals` while
	// private-family and private-starcitizen use `23 Goals`. The configured value is therefore read
	// from vault-cli's own config (see vaultGoalsDir) and this constant is used only when that read
	// fails, with a warning. The tasks directory can be a constant because the task file's own path
	// is what this backend was pointed at, while the goals directory is reached by a name that has
	// to be translated into a path.
	DefaultLocalGoalsDir = "24 Goals"
	// localRoleMapFile is the WezTerm role map, under the home directory. It is written by the
	// WezTerm configuration on its reconcile tick and publishes each window's name, chip colour and
	// window id — the same file claude-supervisor's server/role-map.mjs reads.
	localRoleMapFile = ".cache/wezterm-role-map.json"
	// localDefaultRole is the role a task that declares none is treated as, matching
	// claude-supervisor's server/role-map.mjs DEFAULT_ROLE.
	localDefaultRole = "agent"
	// localColorCommand is the slash command that sets a pane's chip colour, and localColorMarker
	// is the text Claude Code prints when it takes effect.
	//
	// ⚠️ THE MARKER IS THE CONFIRMATION, NOT THE SEND'S EXIT CODE. `wezterm cli send-text` reports
	// that keystrokes reached the pty, and that is true whether or not the message submitted —
	// measured on the supervisor's own path, where a colour reported as applied while it never was
	// (3 of 6 spawns, 2026-09-20). The marker is the pane's own output.
	localColorCommand = "/color "
	localColorMarker  = "Session color set to"
	// localPromptGlyph is the input box's prompt, drawn at the start of the composer line once the
	// TUI is up.
	localPromptGlyph = "❯"
	// The pane-driving timings, expressed as attempt counts rather than a clock, so a wait is a
	// constant rather than a deadline a reader has to reason about: readiness is polled for
	// 40 × 200ms = 8s, and the confirmation for 10 × 100ms = 1s per attempt, retried twice with a
	// bare Enter. Both mirror claude-supervisor's server/tab.mjs, whose values were measured
	// against live panes rather than chosen.
	localPaneReadyAttempts   = 40
	localPaneReadyInterval   = 200 * time.Millisecond
	localConfirmAttempts     = 10
	localConfirmInterval     = 100 * time.Millisecond
	localConfirmRetries      = 2
	localPaneScrollbackLines = 600
)

// taskFrontmatter is the part of a task file's leading YAML block the local backend reads.
//
// ⚠️ IT IS PARSED AS YAML RATHER THAN LINE BY LINE, AND `goals:` IS WHY. The live vault carries
// that field in BOTH shapes — a block list (`goals:` then `    - '[[Name]]'`, 2367 files) and an
// inline flow list (`goals: ['[[Name]]']`, 420) — and a goal NAME itself contains commas
// ("Supervisor Routes Workers to local, docker or k8s Per Task"). A hand-rolled reader that split
// the inline form on its commas would cut that name in two and then look up a goal that does not
// exist, silently falling through to the vault launcher. frontmatterSessionID can read its field
// line by line precisely because that value is a single scalar with no such structure.
type taskFrontmatter struct {
	// Launcher is the `launcher:` the page names for itself, empty when it names none.
	Launcher string
	// Role is the `role:` the page declares, empty when it declares none.
	Role string
	// Goals is the `goals:` list, with each entry's `[[…]]` stripped.
	Goals []string
}

// readTaskFrontmatter reads the task's own file and returns the frontmatter fields the local
// backend resolves from it: the `launcher:` a task may name for itself, the `role:` that decides
// its window and chip colour, and the `goals:` whose launchers are the second rule in the
// precedence.
//
// ⚠️ AN UNREADABLE OR UNPARSEABLE FILE IS NOT AN ERROR HERE, AND THAT IS DELIBERATE RATHER THAN
// LENIENT. The file is read for three separate things and two of them have a documented default —
// the vault's claude_script and the `agent` role — so such a file degrades to exactly the behaviour
// that existed before either field was honoured, while refusing would turn an unreadable file into
// a create that can never succeed. The third read, the session-id baseline, already treats it the
// same way (see taskSessionID), so all three agree. The warning names the task, so the degradation
// is visible rather than silent.
func readTaskFrontmatter(ctx context.Context, vaultDir string, task TaskName) taskFrontmatter {
	path := filepath.Join(expandHome(vaultDir), DefaultLocalTasksDir, string(task)+".md")
	//nolint:gosec // G304: the controller's own configured vault plus a name validateTaskName accepted
	content, err := os.ReadFile(path)
	if err != nil {
		slog.Warn(
			"read task frontmatter failed; using the vault launcher and the default role",
			"task", task,
			"error", err,
		)
		return taskFrontmatter{}
	}
	frontmatter, err := parseTaskFrontmatter(ctx, string(content))
	if err != nil {
		slog.Warn(
			"parse task frontmatter failed; using the vault launcher and the default role",
			"task", task,
			"error", err,
		)
		return taskFrontmatter{}
	}
	return frontmatter
}

// parseTaskFrontmatter parses a page's leading YAML frontmatter block into the three fields the
// local backend reads. Only the FIRST block is parsed: everything after its closing delimiter is
// body text, where a `key: value` line means something else.
func parseTaskFrontmatter(ctx context.Context, content string) (taskFrontmatter, error) {
	block, found := frontmatterBlock(content)
	if !found {
		return taskFrontmatter{}, nil
	}
	var values map[string]any
	if err := yaml.Unmarshal([]byte(block), &values); err != nil {
		return taskFrontmatter{}, errors.Wrap(ctx, err, "parse frontmatter yaml failed")
	}
	return taskFrontmatter{
		Launcher: frontmatterText(values, "launcher"),
		Role:     frontmatterText(values, "role"),
		Goals:    frontmatterTexts(values, "goals"),
	}, nil
}

// frontmatterBlock returns the page's leading YAML frontmatter block, and whether one was found.
// An unterminated block is not one: a page whose first line is `---` and which never closes it has
// no frontmatter, and reading the rest of the page as YAML would invent fields out of body text.
func frontmatterBlock(content string) (string, bool) {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", false
	}
	for index, line := range lines[1:] {
		if strings.TrimSpace(line) == "---" {
			return strings.Join(lines[1:index+1], "\n"), true
		}
	}
	return "", false
}

// frontmatterText returns a frontmatter scalar, trimmed, or the empty string when the key is absent
// or does not hold a string. A list or a nested block under the key is not a scalar, so it answers
// empty rather than a rendered form of itself.
func frontmatterText(values map[string]any, key string) string {
	text, ok := values[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

// frontmatterTexts returns a frontmatter list of strings, each trimmed and unwrapped. A key holding
// anything but a list answers nothing, so a malformed value degrades to "no entries" rather than to
// a bogus goal name.
func frontmatterTexts(values map[string]any, key string) []string {
	entries, ok := values[key].([]any)
	if !ok {
		return nil
	}
	texts := make([]string, 0, len(entries))
	for _, entry := range entries {
		text, ok := entry.(string)
		if !ok {
			continue
		}
		if trimmed := trimWikilink(text); trimmed != "" {
			texts = append(texts, trimmed)
		}
	}
	return texts
}

// trimWikilink strips the `[[` and `]]` a vault wikilink carries, mirroring the supervisor's own
// reader (server/task-launcher.mjs) so a goal name reaches the file lookup as a bare name.
func trimWikilink(value string) string {
	trimmed := strings.TrimSpace(value)
	trimmed = strings.TrimPrefix(trimmed, "[[")
	trimmed = strings.TrimSuffix(trimmed, "]]")
	return strings.TrimSpace(trimmed)
}

// launcherSource names the rule that chose a worker's launcher. It is what the create reply reports
// as `launcher_source`, so a caller can see which rule won rather than having to infer it from the
// path it was given.
type launcherSource string

const (
	// launcherSourceTask means the task named its own `launcher:`.
	launcherSourceTask launcherSource = "task"
	// launcherSourceGoal means the task named none and exactly one of its goals named one.
	launcherSourceGoal launcherSource = "goal"
	// launcherSourceVault means neither named one, so the vault's claude_script is used.
	launcherSourceVault launcherSource = "vault"
)

// resolvedLauncher is the script a worker's tab will exec and the rule that chose it.
type resolvedLauncher struct {
	Path   string
	Source launcherSource
}

// safeLauncher is the shape a `launcher:` frontmatter value must have: a plain script name or path,
// and nothing a shell could read as syntax.
//
// ⚠️ IT IS claude-supervisor's OWN REGEX (server/spawn-cwd.mjs SAFE_LAUNCHER), KEPT IDENTICAL
// RATHER THAN RE-DERIVED. Both spawn sites exec the value inside `bash -lc`, so a rule that admitted
// on one side what the other refuses would make the fleet's behaviour depend on which route opened
// the worker — the exact divergence this change exists to remove.
var safeLauncher = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// resolveTask resolves the two things a create reads out of the task's own file: the launcher its
// tab will exec, and the role that decides the window and the chip colour.
//
// ⚠️ THE FILE IS READ ONCE FOR BOTH, AND BOTH ARE RESOLVED BEFORE ANY TAB IS OPENED. The
// executability check inside the first is why the placement matters: `wezterm cli spawn` reports
// success for a tab whose `exec` then fails, so a bad launcher discovered after the spawn would be
// a recorded worker whose pane is already gone. Neither read is fatal on its own — a file that
// cannot be read degrades to the documented defaults — so this refuses only a value it can see is
// wrong.
func (b *localBackend) resolveTask(
	ctx context.Context,
	task TaskName,
) (resolvedLauncher, resolvedRole, error) {
	frontmatter := readTaskFrontmatter(ctx, b.config.VaultDir, task)
	chosen, err := b.launcherFor(ctx, frontmatter)
	if err != nil {
		return resolvedLauncher{}, resolvedRole{}, err
	}
	role, err := b.resolveRole(ctx, frontmatter.Role)
	if err != nil {
		return resolvedLauncher{}, resolvedRole{}, err
	}
	return chosen, role, nil
}

// launcherFor returns the launcher the worker's tab must exec, having refused any value this
// backend will not run.
func (b *localBackend) launcherFor(
	ctx context.Context,
	frontmatter taskFrontmatter,
) (resolvedLauncher, error) {
	chosen, err := b.chooseLauncher(ctx, frontmatter)
	if err != nil {
		return resolvedLauncher{}, err
	}
	// ⚠️ THE EXECUTABILITY CHECK IS DELIBERATELY STRICTER THAN THE SUPERVISOR'S OWN RESOLUTION, AND
	// THE DIVERGENCE IS INTENDED. `resolveTaskLauncher` validates the value's SHAPE only —
	// SAFE_LAUNCHER and the `..` refusal — because a bad launcher there surfaces as an unreadable
	// `model: null` on the spawn response, an observable that module already reports. This backend
	// has no such observable: `wezterm cli spawn` reports success for a tab whose `exec` then fails,
	// so a typo produces a recorded worker whose pane is already gone — a create that reads as
	// successful and a worker that does not exist. Checking here, before any tab is opened, is what
	// turns that into an error the caller can act on and leaves no pane behind.
	//
	// A bare name that resolved beside the vault launcher is checked as the PATH it became, so the
	// common typo — `cc-private-claud` — is refused here rather than inside a dying pane.
	if err := requireExecutableLauncher(ctx, chosen.Path); err != nil {
		return resolvedLauncher{}, err
	}
	return chosen, nil
}

// chooseLauncher applies the fleet's launcher precedence: the task's `launcher:`, else its goals',
// else the vault's claude_script.
//
// A task naming its own launcher never reads its goals, and that is a rule rather than an
// optimisation: a goal whose file has gone missing must not be able to refuse a spawn the
// precedence says should open.
//
// The path is expanded once, here, so the executability check, the tab's command line and the
// create reply all name the same file. A frontmatter value can never carry a `~` — safeLauncher
// refuses one — so this only ever matters for the vault launcher, which is operator config and may
// legitimately be written the way a shell writes it.
func (b *localBackend) chooseLauncher(
	ctx context.Context,
	frontmatter taskFrontmatter,
) (resolvedLauncher, error) {
	if own := strings.TrimSpace(frontmatter.Launcher); own != "" {
		path, err := launcherPath(ctx, b.config.ClaudeScript, own)
		if err != nil {
			return resolvedLauncher{}, err
		}
		return resolvedLauncher{Path: expandHome(path), Source: launcherSourceTask}, nil
	}
	fromGoals, err := b.goalLauncher(ctx, frontmatter.Goals)
	if err != nil {
		return resolvedLauncher{}, err
	}
	if fromGoals != "" {
		path, err := launcherPath(ctx, b.config.ClaudeScript, fromGoals)
		if err != nil {
			return resolvedLauncher{}, err
		}
		return resolvedLauncher{Path: expandHome(path), Source: launcherSourceGoal}, nil
	}
	return resolvedLauncher{
		Path:   expandHome(b.config.ClaudeScript),
		Source: launcherSourceVault,
	}, nil
}

// goalLauncher returns the single launcher the task's goals name, or the empty string when none of
// them names one.
//
// ⚠️ TWO GOALS NAMING DIFFERENT LAUNCHERS IS REFUSED, NOT RESOLVED BY ORDER. Frontmatter list order
// is not a precedence anyone chose, so picking the first would be a silent guess — the failure class
// this whole rule exists to remove. The refusal names the values it found and the field that settles
// it.
//
// ⚠️ A GOAL THAT CANNOT BE READ IS SKIPPED WITH A WARNING, NEVER AN ERROR. A stale goal link is a
// normal vault state — a goal renamed or archived while a task still links it — and refusing on one
// would stop every task carrying it from opening at all. The supervisor's own reader
// (server/task-launcher.mjs) applies the same rule.
func (b *localBackend) goalLauncher(ctx context.Context, names []string) (string, error) {
	if len(names) == 0 {
		return "", nil
	}
	goalsDir := b.goalsDir(ctx)
	seen := make(map[string]bool, len(names))
	values := make([]string, 0, len(names))
	for _, name := range names {
		value, err := readGoalLauncher(ctx, b.config.VaultDir, goalsDir, name)
		if err != nil {
			slog.Warn("goal skipped for launcher resolution", "goal", name, "error", err)
			continue
		}
		if value == "" || seen[value] {
			continue
		}
		// ⚠️ EVERY VALUE IS SHAPE-CHECKED BEFORE THE COUNT IS, so an unsafe launcher is refused for
		// what it IS rather than losing a count comparison to a sibling goal. This is the order the
		// supervisor's resolver uses, and it is what makes the refusal name the real problem: a task
		// with two goals, one of them `../evil`, is a task that tried to exec outside the launcher
		// directory, not a task with an ambiguous launcher.
		if _, err := launcherPath(ctx, b.config.ClaudeScript, value); err != nil {
			return "", err
		}
		seen[value] = true
		values = append(values, value)
	}
	switch len(values) {
	case 0:
		return "", nil
	case 1:
		return values[0], nil
	default:
		return "", errors.Errorf(
			ctx,
			"the task's goals name different launchers (%s) and the task names none — "+
				"refusing rather than picking one by frontmatter order; set `launcher:` on the task "+
				"to choose",
			strings.Join(values, ", "),
		)
	}
}

// readGoalLauncher returns the launcher the named goal records for itself, or the empty string when
// it records none.
//
// ⚠️ THE GOAL NAME BECOMES A PATH COMPONENT, so it is validated as one, exactly as a task name is.
// Frontmatter is writable by supervised workers, and a name carrying `..` or a separator would make
// the controller read an arbitrary file and act on whatever `launcher:` it holds. A name that fails
// this is an error rather than a skip, so the caller's warning names it.
func readGoalLauncher(
	ctx context.Context,
	vaultDir string,
	goalsDir string,
	name string,
) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" || strings.ContainsAny(trimmed, `/\`) || strings.Contains(trimmed, "..") {
		return "", errors.Errorf(ctx, "goal %q is not a valid goal name", name)
	}
	path := filepath.Join(expandHome(vaultDir), goalsDir, trimmed+".md")
	//nolint:gosec // G304: the controller's own configured vault plus a validated goal name
	content, err := os.ReadFile(path)
	if err != nil {
		return "", errors.Wrapf(ctx, err, "read goal file %q failed", path)
	}
	frontmatter, err := parseTaskFrontmatter(ctx, string(content))
	if err != nil {
		return "", errors.Wrapf(ctx, err, "parse goal file %q failed", path)
	}
	return frontmatter.Launcher, nil
}

// launcherPath turns a `launcher:` frontmatter value into the path the tab execs, refusing any value
// that is not a plain script name or path.
//
// ⚠️ THE SHAPE CHECK IS A TRUST-BOUNDARY GUARD, NOT A STYLE PREFERENCE. Before this field existed
// the launcher came only from operator-controlled vault-cli config; it now comes from task and goal
// frontmatter, which supervised workers can write, and tabCommand execs the value inside
// `bash -lc`. Anything but a plain path — shell metacharacters, whitespace, a `..` segment — is
// therefore REFUSED rather than quoted or stripped, so a hostile or mistyped value never reaches a
// shell at all.
//
// A bare name resolves in the SAME DIRECTORY as the vault's own launcher — the cc-* scripts live
// side by side — and a value containing `/` is used as given. The value is deliberately NOT checked
// against a list of known launchers: a typo is caught by the executability check that follows, which
// is an observable this backend has and the supervisor's response shape does not.
func launcherPath(ctx context.Context, vaultLauncher string, value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if !safeLauncher.MatchString(trimmed) {
		return "", errors.Errorf(
			ctx,
			"launcher %q is not a plain script name or path — refusing rather than exec'ing it, "+
				"since frontmatter is writable by workers. Use letters, digits, `.`, `_`, `-` and "+
				"`/` only",
			value,
		)
	}
	// ⚠️ A SEPARATE CHECK, BECAUSE THE REGEX DOES NOT COVER IT: `..` is built from characters
	// safeLauncher allows, so `../../bin/sh` passes the shape test and is refused only here.
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == ".." {
			return "", errors.Errorf(
				ctx,
				"launcher %q carries a %q path segment — refusing rather than exec'ing a value that "+
					"escapes the launcher directory",
				value,
				"..",
			)
		}
	}
	if strings.Contains(trimmed, "/") {
		return trimmed, nil
	}
	dir := filepath.Dir(vaultLauncher)
	if dir == "." {
		return trimmed, nil
	}
	return filepath.Join(dir, trimmed), nil
}

// requireExecutableLauncher refuses a launcher that is not an executable regular file.
//
// A directory is refused as well as a non-executable file: `exec` on a directory fails at the shell,
// and a mode check alone would admit one whose permissions happen to carry an execute bit.
func requireExecutableLauncher(ctx context.Context, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return errors.Wrapf(ctx, err, "launcher %q is not an executable file", path)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return errors.Errorf(ctx, "launcher %q is not an executable file", path)
	}
	return nil
}

// goalsDir returns the vault directory a goal file lives in.
//
// ⚠️ IT IS DERIVED FROM vault-cli's OWN CONFIG RATHER THAN HARDCODED, AND THE FALLBACK IS ANNOUNCED.
// The directory is a vault-cli setting, not a vault invariant, and a wrong one is not a visible
// failure: the goal rule simply finds nothing and the worker opens on the vault default — which is
// the very silent drop this change exists to remove, reintroduced one layer down. vault-cli is the
// single home of that setting, and the supervisor resolves its goal launchers through it
// (server/task-launcher.mjs shells out to `vault-cli goal get`), so reading the same config is what
// keeps the two spawn sites agreeing. The fallback is bounded for the same reason: being wrong there
// costs the goal rule, never a refusal.
func (b *localBackend) goalsDir(ctx context.Context) string {
	dir, err := vaultGoalsDir(ctx, b.config.VaultDir)
	if err != nil {
		slog.Warn(
			"resolve the vault's goals directory failed; falling back to the default",
			"vault", b.config.VaultDir,
			"goals_dir", DefaultLocalGoalsDir,
			"error", err,
		)
		return DefaultLocalGoalsDir
	}
	return dir
}

// vaultGoalsDir returns the goals directory vault-cli's own config records for the vault at
// vaultDir.
//
// ⚠️ THE VAULT IS MATCHED BY PATH, NOT BY NAME, AND THAT IS FORCED RATHER THAN CHOSEN. This backend
// is configured with a directory (`-local-vault`), never a vault name, so the config's own `path` is
// the only field there is to compare against; a name-based lookup would need a name this process was
// never given.
func vaultGoalsDir(ctx context.Context, vaultDir string) (string, error) {
	vaults, err := vaultconfig.NewLoader("").GetAllVaults(ctx)
	if err != nil {
		return "", errors.Wrap(ctx, err, "load vault-cli config failed")
	}
	return goalsDirForVault(ctx, vaults, vaultDir)
}

// goalsDirForVault returns the goals directory these vault-cli entries record for the vault at
// vaultDir.
//
// It is split out from the load so the matching rule is testable against a literal entry list
// rather than against whatever config happens to be installed on the machine running the spec —
// which is the one thing about this rule worth pinning, since matching on the wrong field would
// silently resolve every goal of every vault to one directory.
func goalsDirForVault(
	ctx context.Context,
	vaults []*vaultconfig.Vault,
	vaultDir string,
) (string, error) {
	wanted := filepath.Clean(expandHome(vaultDir))
	for _, vault := range vaults {
		if filepath.Clean(vault.Path) == wanted {
			return vault.GetGoalsDir(), nil
		}
	}
	return "", errors.Errorf(ctx, "no vault-cli config entry names the vault %q", vaultDir)
}

// localRoles is the vocabulary a task's `role:` frontmatter may use. An unknown role is REFUSED
// rather than defaulted, mirroring claude-supervisor's server/role-map.mjs: a worker opened in the
// wrong window with the wrong colour is discovered only by noticing it.
var localRoles = []string{"manager", "agent", "human"}

// localRolePurposes maps a role to the window name the role map uses for it. The two vocabularies
// are deliberately different — a window named "Direct" serves the role "human" — so this is the one
// place that translates between them, exactly as server/role-map.mjs does.
var localRolePurposes = map[string]string{
	"manager": "Managers",
	"agent":   "Agents",
	"human":   "Direct",
}

// roleMapEntry is one window's entry in the WezTerm role map.
//
// ⚠️ BOTH FIELDS ARE POINTERS, SO AN ABSENT OR NULL VALUE IS DISTINGUISHABLE FROM A ZERO ONE, AND
// BOTH DISTINCTIONS ARE LOAD-BEARING. The live map publishes `"Hold": {"chip": null, ...}`, and a
// plain string would read that null as the empty colour — the same value an absent key produces —
// while a plain int would read a missing `window_id` as window 0, which is the Managers window.
// Reading a dropped field as a valid window id is how a worker lands in the wrong window with
// nothing to show for it.
type roleMapEntry struct {
	Chip     *string `json:"chip"`
	WindowID *int    `json:"window_id"`
}

// roleMap is the parsed contents of the WezTerm role map, keyed by window name.
type roleMap map[string]roleMapEntry

// resolvedRole is what a task's `role:` resolves to: the chip colour to apply and the window to open
// the tab in, or Resolved false when the map held no usable entry for the role.
type resolvedRole struct {
	// Role is the role that was resolved, after the default was applied. It is carried so a warning
	// can name it without the caller re-deriving it.
	Role string
	// Chip is the chip colour to send into the pane, empty when the role map publishes none.
	Chip string
	// WindowID is the WezTerm window to open the tab in, empty when unresolved.
	WindowID string
	// Resolved reports whether the role map answered. False means the caller must fall back and say
	// so, rather than reading an empty WindowID as "no preference".
	Resolved bool
}

// resolveRole returns the window and chip a task's role resolves to.
//
// ⚠️ THE TWO FAILURE MODES ARE DELIBERATELY NOT THE SAME KIND OF FAILURE, mirroring
// claude-supervisor's server/role-map.mjs:
//
//   - an unknown ROLE is refused. It is a caller mistake, and the consequence of guessing is a
//     worker opened in the wrong window with the wrong colour, which is discovered only by noticing
//     it.
//   - an unusable MAP is degraded, not refused. The map is published on WezTerm's own reconcile
//     tick, so it is absent on a machine that has never published one and briefly absent after a
//     restart; refusing would make this backend unusable there. The caller falls back to the
//     configured window, applies no colour, and SAYS SO in a log — a spawn that looks routed and is
//     not is worse than one that plainly is not.
func (b *localBackend) resolveRole(ctx context.Context, role string) (resolvedRole, error) {
	wanted, err := validateRole(ctx, role)
	if err != nil {
		return resolvedRole{}, err
	}
	path, err := b.roleMapPath(ctx)
	if err != nil {
		slog.Warn(
			"resolve the wezterm role map path failed; opening without a role window or colour",
			"role", wanted,
			"error", err,
		)
		return resolvedRole{Role: wanted}, nil
	}
	entries, err := readRoleMap(ctx, path)
	if err != nil {
		slog.Warn(
			"read the wezterm role map failed; opening without a role window or colour",
			"role", wanted,
			"path", path,
			"error", err,
		)
		return resolvedRole{Role: wanted}, nil
	}
	return roleFromMap(wanted, entries), nil
}

// roleWindowID returns the window a worker's tab must open in: the role's own when the map resolved
// one, else the configured value.
//
// ⚠️ AN UNRESOLVED ROLE IS NOT "NO PREFERENCE", AND THE LOG IS WHAT STOPS IT READING AS ONE. A role
// the map could not answer leaves the resolved window empty, and passing that through would land
// the tab in whatever window the spawning process happens to be associated with — which under
// launchd is the default window, and was measured landing an `agent` row in the Managers window.
// The fallback is the configured window, announced rather than silent, because a spawn that looks
// routed and is not is worse than one that plainly is not.
func (b *localBackend) roleWindowID(task TaskName, role resolvedRole) string {
	if role.Resolved {
		return role.WindowID
	}
	slog.Warn(
		"the wezterm role map has no usable entry for the role; opening in the configured window "+
			"and applying no colour",
		"task", task,
		"role", role.Role,
		"window", b.config.WindowID,
	)
	return b.config.WindowID
}

// validateRole returns the role to resolve, applying the default when the task declares none, and
// refuses a role this backend does not know.
func validateRole(ctx context.Context, role string) (string, error) {
	wanted := strings.TrimSpace(role)
	if wanted == "" {
		wanted = localDefaultRole
	}
	if !slices.Contains(localRoles, wanted) {
		return "", errors.Errorf(
			ctx,
			"role %q is not a role — refusing rather than opening a worker in the wrong window with "+
				"the wrong colour. Valid values: %s (or omit the field for %q)",
			role,
			strings.Join(localRoles, ", "),
			localDefaultRole,
		)
	}
	return wanted, nil
}

// roleFromMap looks the role's window up in the map and returns what it found.
//
// A window id is rendered as a string because that is what `wezterm cli spawn --window-id` takes;
// the map publishes it as a JSON number, and strconv is the conversion rather than a format string,
// so nothing but an integer can reach the flag.
func roleFromMap(role string, entries roleMap) resolvedRole {
	entry, found := entries[localRolePurposes[role]]
	if !found || entry.WindowID == nil {
		return resolvedRole{Role: role}
	}
	resolved := resolvedRole{
		Role:     role,
		WindowID: strconv.Itoa(*entry.WindowID),
		Resolved: true,
	}
	if entry.Chip != nil {
		resolved.Chip = strings.TrimSpace(*entry.Chip)
	}
	return resolved
}

// roleMapPath returns the role map's path: the configured override when one is set, else
// ~/.cache/wezterm-role-map.json.
//
// The override exists so a spec can drive role resolution against a temporary map; the default is
// the file the WezTerm configuration actually writes. It is resolved per create rather than cached,
// because the map is republished whenever that configuration reloads.
func (b *localBackend) roleMapPath(ctx context.Context) (string, error) {
	if path := strings.TrimSpace(b.config.RoleMapPath); path != "" {
		return expandHome(path), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.Wrap(ctx, err, "resolve home directory failed")
	}
	return filepath.Join(home, localRoleMapFile), nil
}

// readRoleMap parses the role map at path.
func readRoleMap(ctx context.Context, path string) (roleMap, error) {
	//nolint:gosec // G304: the role map is WezTerm's own published file at a fixed path
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.Wrapf(ctx, err, "read wezterm role map %q failed", path)
	}
	var entries roleMap
	if err := json.Unmarshal(content, &entries); err != nil {
		return nil, errors.Wrapf(ctx, err, "parse wezterm role map %q failed", path)
	}
	return entries, nil
}

// applyRoleColor sends the role's chip colour into the worker's new pane and confirms it landed.
//
// ⚠️ NON-FATAL, AND THAT IS THE SUPERVISOR'S OWN RULE: a colour that fails to apply leaves a worker
// that still has its task, so failing the create over it would orphan a live pane the caller was
// never told about — and would do it for a purely cosmetic difference. The failure is logged with
// the pane and the chip, so an uncoloured tab is diagnosable rather than mysterious.
//
// A role the map publishes no chip for (the map's own `Hold` entry is such a role) sends nothing at
// all rather than a default: a wrong role signal is worse than a missing one.
func (b *localBackend) applyRoleColor(
	ctx context.Context,
	guiID string,
	paneID string,
	role resolvedRole,
) {
	if role.Chip == "" {
		return
	}
	if err := b.client.SendText(
		ctx,
		guiID,
		paneID,
		localColorCommand+role.Chip,
		localColorMarker,
	); err != nil {
		slog.Warn(
			"apply the role's chip colour to the worker pane failed",
			"pane", paneID,
			"role", role.Role,
			"chip", role.Chip,
			"error", err,
		)
	}
}
