// scope.go reads and amends .scope, the issues a workspace covers. `facet
// spawn` writes it; this is for a workspace handed another issue after it
// already exists, or one whose recorded scope turned out wrong.

package main

import (
	"fmt"
	"io"
	"os"

	"github.com/RiccardoCereghino/facet/internal/config"
	"github.com/RiccardoCereghino/facet/internal/scope"
	"github.com/RiccardoCereghino/facet/internal/workspace"
	"github.com/spf13/cobra"
)

func newScopeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scope",
		Short: "Show or change the issues a workspace covers",
		Long: "A workspace records the issues it covers in .scope, one owner/repo#n per\n" +
			"line. `facet spawn` writes it; this reads it and changes it afterwards.\n\n" +
			"Every subcommand finds the workspace by walking up from the working\n" +
			"directory, so it works from inside a clone. A workspace with no .scope has\n" +
			"no recorded scope, which is a normal state.",
	}
	cmd.AddCommand(newScopeListCmd(), newScopeAddCmd(), newScopeRemoveCmd(), newScopeSetCmd())
	return cmd
}

func newScopeListCmd() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Print the issues this workspace covers",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			ws, err := config.FindWorkspace(path)
			if err != nil {
				return err
			}
			return runScopeList(os.Stdout, ws)
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "start the search here instead of the working directory")
	return cmd
}

func runScopeList(w io.Writer, ws string) error {
	refs, err := scope.ReadScope(ws)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "workspace: %s\n", ws)
	if len(refs) == 0 {
		_, _ = fmt.Fprintf(w, "scope:     (none recorded in %s)\n", scope.ScopeFile)
		return nil
	}
	_, _ = fmt.Fprintln(w, "scope:")
	for _, r := range refs {
		_, _ = fmt.Fprintf(w, "  %s\n", r)
	}
	return nil
}

func newScopeAddCmd() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "add <owner/repo#n>...",
		Short: "Record another issue this workspace covers",
		Long: "Appends to .scope, creating it if the workspace has none. Adding an entry\n" +
			"already recorded changes nothing, so this is safe to repeat.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			refs, err := scope.ParseRefs(args)
			if err != nil {
				return err
			}
			ws, err := config.FindWorkspace(path)
			if err != nil {
				return err
			}
			added, err := scope.AppendScope(ws, refs)
			if err != nil {
				return err
			}
			rep := workspace.Reporter{W: os.Stdout}
			for _, r := range refs {
				if containsRef(added, r) {
					rep.Created("%s: %s", scope.ScopeFile, r)
				} else {
					rep.Unchanged("%s: %s (already recorded)", scope.ScopeFile, r)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "start the search here instead of the working directory")
	return cmd
}

func containsRef(refs []scope.Ref, want scope.Ref) bool {
	for _, r := range refs {
		if r == want {
			return true
		}
	}
	return false
}

// newScopeRemoveCmd is the opposite of add (facet#112): a boundary that can
// only widen is a ratchet, and a wrong entry was permanent -- correcting one
// meant editing .scope by hand, exactly what these tools exist to make
// unnecessary. Removing something not present is a no-op, not a refusal, for
// the same idempotence reason add has.
func newScopeRemoveCmd() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "remove <owner/repo#n>...",
		Short: "Drop an entry this workspace no longer covers",
		Long: "Rewrites .scope without the given entries, printing what remains.\n\n" +
			"Removing an entry not present changes nothing, so this is safe to repeat.\n" +
			"If nothing is left, .scope is deleted rather than left empty -- absent means\n" +
			"nothing recorded, the same rule `facet spawn` and `scope add` already follow.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			refs, err := scope.ParseRefs(args)
			if err != nil {
				return err
			}
			ws, err := config.FindWorkspace(path)
			if err != nil {
				return err
			}
			removed, remaining, err := scope.RemoveScope(ws, refs)
			if err != nil {
				return err
			}
			rep := workspace.Reporter{W: os.Stdout}
			for _, r := range refs {
				if containsRef(removed, r) {
					rep.Pruned("%s: dropped %s", scope.ScopeFile, r)
				} else {
					rep.Unchanged("%s: %s (was not recorded)", scope.ScopeFile, r)
				}
			}
			printScopeAfter(os.Stdout, remaining)
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "start the search here instead of the working directory")
	return cmd
}

// newScopeSetCmd replaces the scope wholesale, printing the previous value so
// an edit that turns out wrong is recoverable from the command's own output.
func newScopeSetCmd() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "set <owner/repo#n>...",
		Short: "Replace the scope wholesale, printing what it replaced",
		Long: "Unlike add and remove, this always writes: passing the current scope back\n" +
			"unchanged still counts as a set. Pass no arguments to clear .scope entirely.",
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			refs, err := scope.ParseRefs(args)
			if err != nil {
				return err
			}
			ws, err := config.FindWorkspace(path)
			if err != nil {
				return err
			}
			previous, err := scope.SetScope(ws, refs)
			if err != nil {
				return err
			}
			if len(previous) == 0 {
				_, _ = fmt.Fprintf(os.Stdout, "%s: was empty\n", scope.ScopeFile)
			} else {
				_, _ = fmt.Fprintf(os.Stdout, "%s: replaced %s\n", scope.ScopeFile, scope.Join(previous))
			}
			printScopeAfter(os.Stdout, scope.Dedupe(refs))
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "start the search here instead of the working directory")
	return cmd
}

// printScopeAfter states the scope a remove or set left behind.
func printScopeAfter(w io.Writer, refs []scope.Ref) {
	if len(refs) == 0 {
		_, _ = fmt.Fprintf(w, "scope:     (none recorded)\n")
		return
	}
	_, _ = fmt.Fprintln(w, "scope now:")
	for _, r := range refs {
		_, _ = fmt.Fprintf(w, "  %s\n", r)
	}
}
