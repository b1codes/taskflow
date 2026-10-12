package cli

import (
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
working, and exits 0. When the input has stop_hook_active (the agent is already
continuing because of this hook), it allows the stop, so it can't loop:

  "hooks": {"Stop": [{"hooks": [{"type": "command",
    "command": "taskflow check --project \"$CLAUDE_PROJECT_DIR\" --hook claude"}]}]}

For any agent, a git pre-push hook keeps unfinished work off the remote
(.git/hooks/pre-push, executable):

  #!/bin/sh
  exec taskflow check --project "$(git rev-parse --show-toplevel)"`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if checkSession != "" && checkProject != "" {
			return errors.New("give --session or --project, not both")
		}
		var hook *claudeHookInput
		switch checkHook {
		case "":
		case "claude":
			in, err := readClaudeHookInput(cmd.InOrStdin())
			if err != nil {
				return err
			}
			if in.StopHookActive {
				return nil // the agent is already continuing for this hook
			}
			hook = in
		default:
			return fmt.Errorf("unknown --hook %q: supported hooks are: claude", checkHook)
		}

		sessionID := checkSession
		if sessionID == "" {
			project := checkProject
			if project == "" && hook != nil {
				project = hook.CWD
			}
			if project == "" {
				wd, err := os.Getwd()
				if err != nil {
					return err
				}
				project = wd
			}
			lock, err := session.ReadLockInfo(project)
			if err != nil {
				return fmt.Errorf("failed to read the session lock in %s: %w", project, err)
			}
			if lock == nil {
				if hook == nil {
					fmt.Printf("No taskflow session in %s: nothing to check.\n", project)
				}
				return nil
			}
			sessionID = lock.SessionID
		}

		cfg := ConfigFromContext(cmd.Context())
		if cfg == nil {
			return fmt.Errorf("config not found in context")
		}
		st, err := store.New(cfg.DBPath())
		if err != nil {
			return fmt.Errorf("failed to open database: %w", err)
		}
		defer st.Close()

		res, err := newSessionService(st, cfg).Check(cmd.Context(), sessionID, hook != nil)
		if errors.Is(err, session.ErrSessionNotFound) && checkSession == "" {
			// A lock left by a session this database doesn't know.
			return nil
		}
		if err != nil {
			return err
		}

		switch {
		case hook != nil:
			if !res.Blocked {
				return nil
			}
			return json.NewEncoder(os.Stdout).Encode(claudeHookOutput{Decision: "block", Reason: res.Reason()})
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
	},
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
// stdin) is an empty input.
func readClaudeHookInput(r io.Reader) (*claudeHookInput, error) {
	in := &claudeHookInput{}
	if f, ok := r.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			return in, nil
		}
	}
	data, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to read the hook input: %w", err)
	}
	if len(data) == 0 {
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

	rootCmd.AddCommand(checkCmd)
}
