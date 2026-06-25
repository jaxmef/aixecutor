// Command aixecutor orchestrates AI coding agents through a
// plan → execute → review pipeline. This entrypoint stays trivial; all
// logic lives under internal/.
package main

import (
	"os"

	"github.com/jaxmef/aixecutor/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
