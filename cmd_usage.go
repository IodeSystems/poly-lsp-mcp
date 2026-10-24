package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/iodesystems/poly-lsp-mcp/daemon"
	"github.com/iodesystems/poly-lsp-mcp/mcp"
)

// usageCmd totals the node_query usage log: how often a query returned
// gitignored files or files in a skipped directory, and how often it named
// them. It is the evidence for what the query tree should skip.
func usageCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Summarize node_query usage: how often results come from gitignored or skipped directories",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the summary as JSON")
	return cmd
}
