package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/RiccardoCereghino/facet/internal/ghx"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// newPreflightCmd checks the credential surface every command here shares.
//
// Like newVersionCmd it overrides the root's config-loading pre-run with a
// no-op. A credential check must work anywhere -- outside a workspaces root,
// with a broken routing.json, on a machine where nothing else is set up. The
// thing you reach for when the lab is wrong cannot require the lab to be right.
func newPreflightCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "preflight",
		Short: "Check the GitHub credential facet depends on",
		Long: "Reports whether this machine holds a usable GitHub credential: logged in and\n" +
			"active, the right account, a token type that cannot be rotated out from under\n" +
			"us, the scopes facet actually calls, git talking SSH, and the push key\n" +
			"present.\n\n" +
			"It reads `gh auth status` and nothing else. That is the point: gh reports a\n" +
			"credential as invalid WITHOUT a valid credential, so this check does not go\n" +
			"blind during the fault it exists to catch. No token value is ever read,\n" +
			"printed or stored, and this command never logs in, out, or refreshes.\n\n" +
			"Nothing else requires this to pass: `facet spawn` runs the same checks and\n" +
			"warns, and the workspace core does not consult the credential at all.",
		Args:              cobra.NoArgs,
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPreflight(cmd.OutOrStdout())
		},
	}
}

// runPreflight prints one line per check and fails if any problem was found.
func runPreflight(w io.Writer) error {
	req := ghx.DefaultRequirements()
	probs, notes, st, err := preflight(req)
	if err != nil {
		return err
	}

	if st != nil && st.State == ghx.StateConfirmed {
		_, _ = fmt.Fprintf(w, "host        %s\n", orDash(st.Host))
		_, _ = fmt.Fprintf(w, "account     %s\n", orDash(st.Account))
		_, _ = fmt.Fprintf(w, "token type  %s (value never read)\n", orDash(st.TokenType))
		_, _ = fmt.Fprintf(w, "scopes      %s\n", orDash(strings.Join(st.Scopes, ", ")))
		_, _ = fmt.Fprintf(w, "protocol    %s\n", orDash(st.GitProtocol))
		_, _ = fmt.Fprintf(w, "source      %s\n", orDash(st.ConfigSource))
		_, _ = fmt.Fprintln(w)
	}

	// Notes print on the pass path too, and before the verdict. A check that
	// did not run on this platform must never be readable as a check that
	// passed -- that is the difference between a documented carve-out and a
	// lie, and the pass line below is exactly where someone would misread it.
	writeNotes(w, notes)

	if len(probs) == 0 {
		_, _ = fmt.Fprintln(w, passLine(notes))
		return nil
	}
	for _, p := range probs {
		_, _ = fmt.Fprintf(w, "✗ %s\n", p)
	}
	_, _ = fmt.Fprintln(w)
	return fmt.Errorf("credential preflight failed: %d %s", len(probs), plural(len(probs), "problem", "problems"))
}

// passLine states plainly how much of the preflight actually ran, so a green
// tick cannot overstate itself.
func passLine(notes []string) string {
	if len(notes) == 0 {
		return "✓ credential preflight passed"
	}
	return fmt.Sprintf("✓ credential preflight passed — but %d %s did NOT run on this platform (see above)",
		len(notes), plural(len(notes), "check", "checks"))
}

func writeNotes(w io.Writer, notes []string) {
	for _, n := range notes {
		_, _ = fmt.Fprintf(w, "! %s\n", n)
	}
	if len(notes) > 0 {
		_, _ = fmt.Fprintln(w)
	}
}

// checkSSHKey is a package var for the same reason `gh` is: so a test can drive
// the reporting path with another platform's not-applicable notice.
//
// Without it the display rule -- a green tick must never imply a check that did
// not run -- would be unguarded on every host except Windows, since that is the
// only platform where the real check produces a notice. A rule about what the
// operator sees has to be testable wherever the tests run.
var checkSSHKey = ghx.CheckSSHKey

// preflight runs every check and returns the problems, any checks that could
// not be applied on this platform, and the status it read, so callers can
// report all three.
func preflight(req ghx.Requirements) (probs []ghx.Problem, notes []string, st *ghx.AuthStatus, err error) {
	st, err = gh.Auth()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read gh auth status: %w", err)
	}
	probs = ghx.Check(st, req)
	keyProbs, notApplicable := checkSSHKey(req.SSHKey)
	probs = append(probs, keyProbs...)
	if notApplicable != "" {
		notes = append(notes, notApplicable)
	}
	return probs, notes, st, nil
}

// warnPreflight runs the credential checks before a command that talks to
// GitHub, and reports what it finds without refusing.
//
// Only spawn calls it. The workspace core (new, add, sync, restore) never
// does: it works on plain git repositories, and a credential problem there
// surfaces from git at the clone that needs it. spawn still checks first so a
// problem is named up front rather than mid-operation, but the verdict is the
// operator's: a default `gh auth login` token is a reason to know, not a
// reason to stop, and a credential that truly cannot reach GitHub fails at the
// issue lookup, before anything is created.
func warnPreflight(w io.Writer, what string) {
	probs, notes, _, err := preflight(ghx.DefaultRequirements())
	if err != nil {
		_, _ = fmt.Fprintf(w, "! %s: could not check the GitHub credential: %v\n\n", what, err)
		return
	}
	// Printed whether or not anything is wrong: a command that proceeds on a
	// partially-checked credential surface must say which part went unchecked.
	writeNotes(w, notes)
	if len(probs) == 0 {
		return
	}
	for _, p := range probs {
		_, _ = fmt.Fprintf(w, "! %s\n", p)
	}
	_, _ = fmt.Fprintf(w, "\n! %s is proceeding despite %d credential %s; run `facet preflight` for the full report\n\n",
		what, len(probs), plural(len(probs), "problem", "problems"))
}

// deprecateUnsoundFlag keeps --unsound-credential accepted, and hidden, on the
// verbs that used to refuse without it, so scripts that pass it keep working.
func deprecateUnsoundFlag(f *pflag.FlagSet) {
	f.Bool("unsound-credential", false, "no effect: the credential no longer gates this command")
	_ = f.MarkDeprecated("unsound-credential", "the credential no longer gates this command; the flag has no effect")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
