// Command scriven captures deployed infrastructure as Terraform.
package main

import (
	"os"

	"scriven/internal/cli"
)

func main() { os.Exit(cli.Execute()) }
