// Command facet-graph reads and writes the GitHub issue graph: the sub-issue
// hierarchy (`tree`) and blocked-by dependencies (`deps`).
//
// FROZEN. These commands grew for a multi-agent setup that has since been
// retired. They keep building and their tests keep running, but they are not
// actively maintained, and nothing in `facet` depends on them. They live here,
// out of facet's own command list, until a piece of them earns its way back.
package main

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/RiccardoCereghino/facet/internal/config"
	"github.com/RiccardoCereghino/facet/internal/ghx"
	"github.com/spf13/cobra"
)

var (
	roots config.Roots
	gh    ghx.Client = ghx.CLI{}
)

// version is set by the linker for release builds, as in cmd/facet.
var version = "dev"

func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

func main() {
	root := &cobra.Command{
		Use:   "facet-graph",
		Short: "Read and write the GitHub issue graph (frozen: not actively maintained)",
		Long: "facet-graph reads and writes the sub-issue hierarchy (`tree`) and blocked-by\n" +
			"dependencies (`deps`) across repositories.\n\n" +
			"It is frozen: it builds and is tested, but is not actively maintained.",
		Version:       buildVersion(),
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			return loadRoots()
		},
	}
	root.SetVersionTemplate("facet-graph {{.Version}}\n")
	root.AddCommand(newTreeCmd(), newDepsCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "facet-graph:", err)
		os.Exit(exitCodeFor(err))
	}
}

// loadRoots fills the package-level roots from the configuration.
//
// Named rather than inlined into the root command's pre-run because a command
// that needs to report a CONFIGURATION failure differently -- `tree doctor`,
// which must not answer "I found defects" when it never read anything -- has to
// override that pre-run, and an override that re-implemented the loading could
// drift from it.
func loadRoots() error {
	var err error
	roots, err = config.Load()
	return err
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
