package ghx

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Requirements is what must be true of a GitHub credential before any
// command that depends on it runs.
type Requirements struct {
	// Scopes must all be present.
	Scopes []string
	// GitProtocol is the host-level protocol git must be configured to use.
	GitProtocol string
	// Account is the login the credential must belong to. Empty skips the check.
	Account string
	// ForbiddenTokenTypes are credential prefixes that must not be in use, even
	// when they are currently valid.
	ForbiddenTokenTypes []string
	// SSHKey is the private key path git uses to push. Empty skips the check.
	SSHKey string
}

// Problem is one failed check, phrased so that a red at 3am explains itself
// without anyone having to find the incident it came from. Why is not optional:
// a check whose reason is not written down is a check the next operator deletes.
type Problem struct {
	Check string // what was checked
	Want  string // what is required
	Got   string // what is actually there
	Why   string // the incident or invariant the check exists for
}

func (p Problem) String() string {
	return fmt.Sprintf("%s: want %s, got %s\n    why: %s", p.Check, p.Want, p.Got, p.Why)
}

// DefaultRequirements is the standing requirement set this preflight checks
// against. Each entry's reasoning is carried in the Problem it produces.
func DefaultRequirements() Requirements {
	return Requirements{
		Scopes:              []string{"repo", "read:org", "workflow"},
		GitProtocol:         "ssh",
		Account:             os.Getenv("FACET_GH_ACCOUNT"),
		ForbiddenTokenTypes: []string{"gho_"},
		SSHKey:              DefaultSSHKey(),
	}
}

// DefaultSSHKey is the key every push authenticates with. It
// returns "" when the home directory cannot be determined, which skips the
// check rather than inventing a path.
func DefaultSSHKey() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ssh", "id_ed25519")
}

// Check reports every way st falls short of req. An empty result means the
// credential surface is sound.
//
// Nothing here is advisory and nothing here can be skipped. A gate with an
// escape hatch is not a gate: the whole reason this exists is that the fault it
// catches failed silently and late, and was found mid-operation by trying to
// use the credential.
func Check(st *AuthStatus, req Requirements) []Problem {
	var probs []Problem
	add := func(check, want, got, why string) {
		probs = append(probs, Problem{Check: check, Want: want, Got: got, Why: why})
	}

	// A nil status is not evidence of an absent credential -- it is evidence of
	// nothing at all, so it takes the cautious branch with the rest.
	if st != nil && st.State == StateAbsent {
		add("logged in", "a credential for github.com", "none",
			"this is the fault itself: this machine has lost GitHub access "+
				"mid-operation before and found out by trying to use it. gh reports this "+
				"without needing a working token, so it can be checked first.")
		// Everything below reads fields that are meaningless when logged out.
		return probs
	}

	if st == nil || st.State != StateConfirmed {
		// STILL FATAL. If the forge is unreachable, spawn cannot clone or fetch
		// either, so refusing is right. What must not happen is refusing for the
		// WRONG STATED REASON -- a gate that fires on the right condition and
		// blames the wrong thing is a broken gate, not a strict one.
		add("credential confirmed", "gh can confirm the credential with github.com",
			"gh could NOT confirm it"+withReason(verifyFailure(st)),
			"DO NOT REGENERATE THE TOKEN ON THE STRENGTH OF THIS MESSAGE. gh "+
				"prints exactly this when the token is dead AND when it simply "+
				"cannot reach github.com, and it does not distinguish the two -- so "+
				"neither can this check. Your credential may be perfectly fine. "+
				"gh's own advice here ('To re-authenticate, run: gh auth login' / "+
				"'To forget about this account, run: gh auth logout') will DESTROY a "+
				"working credential if the real problem is the network, and this has "+
				"already caused a ~72-second no-credential window from exactly that "+
				"move. Check connectivity FIRST -- `curl -sS "+
				"-o /dev/null -w '%{http_code}' https://api.github.com` -- and only "+
				"reissue if the network is fine.")
		// The X-shaped output carries no Token or scopes lines at all, so every
		// check below would report "none reported" and bury the one finding that
		// matters.
		return probs
	}

	if !st.Active {
		add("active account", "the credential is the active one", "inactive",
			"gh can hold several logins at once; an inactive one is present in the "+
				"config and used by nothing.")
	}

	if req.Account != "" && !strings.EqualFold(st.Account, req.Account) {
		add("account", req.Account, orNone(st.Account),
			"a valid credential for the wrong identity passes every other check and "+
				"writes to the wrong account.")
	}

	for _, bad := range req.ForbiddenTokenTypes {
		if st.TokenType == bad {
			add("token type", "not "+bad, st.TokenType,
				"gho_ is an OAuth-App token and GitHub issues one per (app, user) "+
					"pair, so ANY other `gh auth login` anywhere -- another machine, a "+
					"container, a Codespace -- silently invalidates this one. That is "+
					"the root cause of the 2026-07-27 outage. A classic or fine-grained "+
					"PAT is a distinct credential and cannot be rotated out from under "+
					"this one by someone else's login.")
		}
	}
	if st.TokenType == "" {
		add("token type", "a recognisable credential prefix", "none reported",
			"gh named no token type, so the credential's kind -- and therefore its "+
				"rotation exposure -- cannot be established.")
	}

	// A fine-grained token has permissions, not scopes, and gh cannot list
	// them; the caller reports that as a note (ScopesNotApplicable).
	if missing := missingScopes(st.Scopes, req.Scopes); len(missing) > 0 && ScopesNotApplicable(st) == "" {
		add("token scopes", strings.Join(req.Scopes, ", "), scopesGot(st.Scopes),
			"missing: "+strings.Join(missing, ", ")+". read:org is REQUIRED by "+
				"`gh auth login --with-token` on a classic token, not an optional "+
				"grant. repo carries every issue, PR and search call this credential "+
				"is used for. workflow is needed for any gh-mediated workflow-file operation; "+
				"pushes are SSH so they never surfaced the gap. A valid but "+
				"scope-short token is the hardest failure to attribute, because "+
				"everything else looks green.")
	}

	if req.GitProtocol != "" && st.GitProtocol != req.GitProtocol {
		add("git protocol", req.GitProtocol, orNone(st.GitProtocol),
			"`gh auth login --with-token` SILENTLY flips the host protocol to https. "+
				"It did on 2026-07-27. An https remote plus a scope gap is a push "+
				"failure that surfaces late, at the moment work would otherwise land. "+
				"Note this is the HOST-level value, which is what gh auth status "+
				"reports; `gh config get git_protocol` returns the global default and "+
				"is the wrong source.")
	}

	return probs
}

// fineGrainedPAT is the prefix of a fine-grained personal access token.
const fineGrainedPAT = "github_pat_"

// ScopesNotApplicable returns a note when the scopes check cannot apply to
// st's credential, and "" when it can.
//
// A fine-grained token carries per-repository permissions instead of OAuth
// scopes, so GitHub sends no X-OAuth-Scopes for it and gh reports none. Read as
// scopes, every fine-grained token would fail for lacking all three -- the
// token type the token-type check recommends. Its permissions are not visible
// to `gh auth status`, so the check is reported as not run rather than passed.
// A fine-grained token that does report scopes is checked as usual.
func ScopesNotApplicable(st *AuthStatus) string {
	if st == nil || st.TokenType != fineGrainedPAT || !noScopes(st.Scopes) {
		return ""
	}
	return "token scopes were NOT checked: a fine-grained token (github_pat_) has " +
		"permissions, not scopes, and gh cannot list them. Check them on GitHub " +
		"(Settings > Developer settings > Fine-grained tokens): facet reads and " +
		"files issues, edits comments, reads pull requests and creates branches"
}

// noScopes reports whether gh listed no scopes, either by leaving the line out
// or by printing "none".
func noScopes(scopes []string) bool {
	return len(scopes) == 0 || (len(scopes) == 1 && strings.EqualFold(scopes[0], "none"))
}

// sshKeyWhy is the reason attached to every SSH-key problem.
const sshKeyWhy = "the token authenticates the API; this key authenticates every push " +
	"made with it. Until this preflight existed, nothing checked it, so half the " +
	"credential surface was unmonitored. SSH cannot replace the token -- GitHub's API does " +
	"not accept SSH authentication -- so both must hold, independently."

// CheckSSHKey reports whether the key git actually pushes with is present and
// private: it exists, and its mode bits let neither group nor other read it,
// which is what ssh itself enforces.
//
// # Local-only, deliberately
//
// No network, no agent, no `ssh -T`. The preflight must run when the machine is
// offline or the forge is down, and a gate that cannot be skipped must not be
// able to go red for a reason that is not about this machine's credentials. The
// cost is declared rather than hidden: this proves the key EXISTS and that it
// is not group- or world-readable. It does not prove GitHub still
// accepts it. A live probe belongs elsewhere.
func CheckSSHKey(path string) []Problem {
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return []Problem{{Check: "ssh key", Want: "a private key at " + path,
			Got: "missing (" + err.Error() + ")", Why: sshKeyWhy}}
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return []Problem{{
			Check: "ssh key permissions",
			Want:  "0600 at " + path,
			Got:   fmt.Sprintf("%#o", mode),
			Why:   sshKeyWhy + " ssh refuses to use a key others can read.",
		}}
	}
	return nil
}

// scopeImplies maps a GitHub OAuth scope to the narrower scopes it grants.
// gh auth status reports only the LITERAL scope a token holds -- never the
// scopes GitHub's own hierarchy implies -- so a credential holding admin:org
// reports as missing read:org even though admin:org already grants it.
//
// Walked transitively by addImpliedScopes: an entry's own implied scopes are
// expanded too, in case one of them implies further scopes of its own. None
// in this table currently do, but the walk costs nothing and does not assume
// the table stays flat.
var scopeImplies = map[string][]string{
	"admin:org":        {"write:org", "read:org"},
	"write:org":        {"read:org"},
	"admin:public_key": {"write:public_key", "read:public_key"},
	"admin:repo_hook":  {"write:repo_hook", "read:repo_hook"},
	"admin:org_hook":   {"read:org_hook"},
	"admin:gpg_key":    {"write:gpg_key", "read:gpg_key"},
	"project":          {"read:project"},
	"user":             {"read:user", "user:email", "user:follow"},
	"repo":             {"repo:status", "repo_deployment", "public_repo", "repo:invite", "security_events"},
}

// addImpliedScopes adds every scope that held (already in set) grants
// transitively, per scopeImplies.
func addImpliedScopes(set map[string]bool, held string) {
	for _, implied := range scopeImplies[held] {
		if !set[implied] {
			set[implied] = true
			addImpliedScopes(set, implied)
		}
	}
}

// missingScopes returns the required scopes absent from have, treating
// GitHub's scope hierarchy as satisfying: a broader scope (admin:org)
// satisfies a narrower one it implies (read:org), even though gh's own
// output never states the implied grant.
func missingScopes(have, want []string) []string {
	set := make(map[string]bool, len(have))
	for _, s := range have {
		held := strings.ToLower(strings.TrimSpace(s))
		set[held] = true
		addImpliedScopes(set, held)
	}
	var missing []string
	for _, w := range want {
		if !set[strings.ToLower(w)] {
			missing = append(missing, w)
		}
	}
	sort.Strings(missing)
	return missing
}

func scopesGot(scopes []string) string {
	if len(scopes) == 0 {
		return "none reported"
	}
	return strings.Join(scopes, ", ")
}

// verifyFailure is st's stated reason, tolerating a nil status.
func verifyFailure(st *AuthStatus) string {
	if st == nil {
		return "gh's status could not be read at all"
	}
	return st.VerifyFailure
}

// withReason appends gh's own words when it gave any, so the report quotes the
// tool rather than paraphrasing it.
func withReason(s string) string {
	if s == "" {
		return ""
	}
	return " (gh says: " + s + ")"
}

func orNone(s string) string {
	if s == "" {
		return "none reported"
	}
	return s
}
