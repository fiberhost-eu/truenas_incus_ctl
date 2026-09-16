package cmd

import (
	"github.com/spf13/cobra"
)

var shareCmd = &cobra.Command{
	Use:   "share",
	Short: "Create, list, update or delete NFS, iSCSI or NVMe-oF shares.",
}

func init() {
	rootCmd.AddCommand(shareCmd)
}
