package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/b1codes/taskflow/internal/session"
	"github.com/b1codes/taskflow/internal/store"
	"github.com/spf13/cobra"
)

// checkBlockedExit is check's exit status while required work is open.
const checkBlockedExit = 2

var (
	checkSession string
	checkProject string
	checkHook    string
)

var checkCmd = &cobra.Command{
	Use:   "check",
	Short: "Exit with status 2 while the session has required completeness obligations open",
	Long: `Evaluate the session's completeness and exit with status 2 while required
obligations are open (or completeness can't be checked), 0 otherwise. Meant for
hooks, which run outside the agent, so it can't skip them.

The session is --session, or the active one in --project (default: the current
directory) found through .b1codes/session.lock. Without an active session there
is nothing to check, and it exits 0. Command checks aren't run: a pass recorded
by taskflow verify / tf_verify counts.

--hook claude speaks Claude Code's Stop hook protocol: it reads the hook input
on stdin (whose cwd stands in for --project), and while work is open prints
{"decision": "block", "reason": ...} with the open list, so the agent keeps
working, and exits 0. A failure (an unreadable config, database, lock, or hook
input) blocks the same way, with the error as the reason, since Claude Code
lets the agent stop on any other exit status. When the input has
stop_hook_active (the agent is already continuing because of this hook), it
allows the stop, so it can't loop:

  "hooks": {"Stop": [{"hooks": [{"type": "command",
    "command": "taskflow check --project \"$CLAUDE_PROJECT_DIR\" --hook claude"}]}]}

For any agent, a git pre-push hook keeps unfinished work off the remote
(.git/hooks/pre-push, executable):

  #!/bin/sh
  exec taskflow check --project "$(git rev-parse --show-toplevel)"`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		switch checkHook {
		case "":
			return runCheck(cmd)
		case "claude":
			return runClaudeHook(cmd)
		default:
			return fmt.Errorf("unknown --hook %q: supported hooks are: claude", checkHook)
		}
	},
}

// checkPreRunErr is the root's setup error (e.g. an invalid config) in a
// --hook claude run, held for runClaudeHook to answer as a block rather than
// failing the command.
var checkPreRunErr error

// checkPreRun is check's PersistentPreRunE, replacing the root's: it runs the
// root's setup, but in --hook claude mode holds its error in checkPreRunErr
// instead of returning it, since a hook that exits 1 doesn't stop the agent.
func checkPreRun(cmd *cobra.Command, args []string) error {
	checkPreRunErr = nil
	err := rootCmd.PersistentPreRunE(cmd, args)
	if err != nil && checkHook == "claude" {
		checkPreRunErr = err
		return nil
	}
	return err
}

// runCheck is check without a hook: it prints the result, a block exits 2,
// and an error exits 1.
func runCheck(cmd *cobra.Command) error {
	res, project, err := evaluateCheck(cmd, "")
	if err != nil {
		return err
	}
	switch {
	case res == nil:
		fmt.Printf("No taskflow session in %s: nothing to check.\n", project)
		return nil
	case JSONOutput():
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(res); err != nil {
			return fmt.Errorf("failed to encode JSON: %w", err)
		}
	case res.Blocked:
		fmt.Println(res.Reason())
	case res.Status != session.StatusActive:
		fmt.Printf("Session %s is %s: nothing to check.\n", res.SessionID, res.Status)
	case res.Report == nil:
		fmt.Printf("Session %s: no completeness manifest, nothing to check.\n", res.SessionID)
	default:
		fmt.Printf("✓ Session %s has no required obligations open.\n", res.SessionID)
	}
	if res.Blocked {
		return exitWith(cmd, checkBlockedExit)
	}
	return nil
}

// runClaudeHook is check --hook claude. It exits 0 (unless writing its answer
// fails): silently to allow the stop, or with a block decision. Only
// stop_hook_active and "nothing to check" (no session lock, a lock this
// database doesn't know, a session that isn't active, no manifest) allow a
// stop without a clean check. Every failure blocks with the error as the
// reason, as an unchecked evaluation does, because Claude Code treats any
// other exit status as a non-blocking hook error and lets the agent stop.
//
// Input that can't be read or parsed blocks too, even though stop_hook_active
// can't be seen in it. Claude Code always sends a JSON object (read whole, so
// it isn't truncated), so such input means the hook isn't getting what Claude
// Code sends, and allowing the stop would silently turn the gate off. Empty
// input already blocks on open work the same way.
func runClaudeHook(cmd *cobra.Command) error {
	in, err := readClaudeHookInput(cmd.InOrStdin())
	if err == nil && in.StopHookActive {
		return nil // the agent is already continuing for this hook
	}
	if err == nil {
		err = checkPreRunErr
	}
	var reason string
	if err == nil {
		var res *session.CheckResult
		res, _, err = evaluateCheck(cmd, in.CWD)
		if err == nil {
			if res == nil || !res.Blocked {
				return nil
			}
			reason = res.Reason()
		}
	}
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "taskflow check: %v\n", err)
		reason = hookErrorReason(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(claudeHookOutput{Decision: "block", Reason: reason}); err != nil {
		return fmt.Errorf("failed to write the hook output: %w", err)
	}
	return nil
}

// hookErrorReason is the block reason for a check that failed: what went
// wrong, and what the agent can do about it.
func hookErrorReason(err error) string {
	return fmt.Sprintf("Taskflow couldn't check this session's completeness: %v. Required work may still be open, so this stop is blocked. "+
		"If the cause is in the project or its taskflow setup (e.g. an invalid .agents/completeness.yaml, .taskflow/completeness.yaml, or ~/.taskflow/config.toml), fix it and run `taskflow check` to confirm; "+
		"otherwise tell the user that taskflow's completeness check is failing and why. Then finish.", err)
}

// evaluateCheck checks the session named by --session, or else the active one
// in --project, hookCWD, or the current directory, in that order. A nil
// result with no error means there is nothing to check: no session lock in
// the project (which it returns), or a lock left by a session this database
// doesn't know.
func evaluateCheck(cmd *cobra.Command, hookCWD string) (*session.CheckResult, string, error) {
	if checkSession != "" && checkProject != "" {
		return nil, "", errors.New("give --session or --project, not both")
	}
	sessionID, project := checkSession, ""
	if sessionID == "" {
		project = checkProject
		if project == "" {
			project = hookCWD
		}
		if project == "" {
			wd, err := os.Getwd()
			if err != nil {
				return nil, "", err
			}
			project = wd
		}
		lock, err := session.ReadLockInfo(project)
		if err != nil {
			return nil, project, fmt.Errorf("failed to read the session lock in %s: %w", project, err)
		}
		if lock == nil {
			return nil, project, nil
		}
		sessionID = lock.SessionID
	}

	cfg := ConfigFromContext(cmd.Context())
	if cfg == nil {
		return nil, project, fmt.Errorf("config not found in context")
	}
	st, err := store.New(cfg.DBPath())
	if err != nil {
		return nil, project, fmt.Errorf("failed to open database: %w", err)
	}
	defer st.Close()

	res, err := newSessionService(st, cfg).Check(cmd.Context(), sessionID, checkHook != "")
	if errors.Is(err, session.ErrSessionNotFound) && checkSession == "" {
		// A lock left by a session this database doesn't know.
		return nil, project, nil
	}
	if err != nil {
		return nil, project, err
	}
	return res, project, nil
}

// claudeHookInput is the part of Claude Code's Stop hook input check reads.
type claudeHookInput struct {
	CWD            string `json:"cwd"`
	StopHookActive bool   `json:"stop_hook_active"`
}

// claudeHookOutput is Claude Code's Stop hook response that keeps the agent
// working, with reason as its next instruction.
type claudeHookOutput struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// readClaudeHookInput reads the hook input; none (a terminal, or empty
// stdin) is an empty input. It reads the input whole: a truncated input
// wouldn't parse, and input that doesn't parse blocks.
func readClaudeHookInput(r io.Reader) (*claudeHookInput, error) {
	in := &claudeHookInput{}
	if f, ok := r.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			return in, nil
		}
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read the hook input: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return in, nil
	}
	if err := json.Unmarshal(data, in); err != nil {
		return nil, fmt.Errorf("invalid hook input: %w", err)
	}
	return in, nil
}

func init() {
	checkCmd.Flags().StringVar(&checkSession, "session", "", "Session to check")
	checkCmd.Flags().StringVar(&checkProject, "project", "", "Project whose active session to check, via .b1codes/session.lock (default: the current directory)")
	checkCmd.Flags().StringVar(&checkHook, "hook", "", "Answer in an agent tool's hook format: claude")
	// Cobra runs only the nearest PersistentPreRunE, so this replaces the
	// root's for check; checkPreRun calls it.
	checkCmd.PersistentPreRunE = checkPreRun

	rootCmd.AddCommand(checkCmd)
}
