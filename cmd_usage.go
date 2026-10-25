package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"

	"github.com/iodesystems/poly-lsp-mcp/daemon"
	"github.com/iodesystems/poly-lsp-mcp/mcp"
)

// usageCmd totals the node_query usage log: how often a query returned
// gitignored files or files in a skipped directory, and how often it named
// them. It is the evidence for what the query tree should skip.
func usageCmd() *cobra.Command {
	var asJSON bool
	var daemonHome func() (string, error)
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Summarize node_query and node_edit usage",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := daemonHome(); err != nil {
				return err
			}
			path := daemon.UsagePath()
			sum, err := mcp.SummarizeUsage(path)
			if errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "no usage recorded yet (looked in %s)\n", path)
				return nil
			}
			if err != nil {
				return err
			}
			if asJSON {
				b, _ := json.MarshalIndent(sum, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			fmt.Printf("%d node_query calls (%s)\n", sum.Queries, path)
			fmt.Printf("  %d returned a gitignored match; %d of those named its path\n", sum.WithIgnored, sum.IgnoredByName)
			fmt.Printf("  %d returned a match in a skipped directory (node_modules, vendor…), reached by naming it\n", sum.WithSkipped)
			fmt.Printf("  %d of %d matches were in gitignored paths\n", sum.IgnoredMatches, sum.AllMatches)
			for _, ex := range sum.IgnoredExamples {
				fmt.Printf("    e.g. %s\n", ex)
			}
			fmt.Printf("%d node_edit calls\n", sum.Edits)
			for _, op := range sortedKeys(sum.EditOps) {
				fmt.Printf("  %-8s %5d (%d failed)\n", op, sum.EditOps[op], sum.EditFailed[op])
			}
			for _, h := range sortedKeys(sum.EditHidden) {
				fmt.Printf("  hidden %s: %d\n", h, sum.EditHidden[h])
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the summary as JSON")
	daemonHome = daemonHomeFlag(cmd.Flags())
	return cmd
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
