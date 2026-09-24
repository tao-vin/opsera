package main

import (
	"log"
	"os"

	"github.com/tao-vin/opsera/internal/app"
	"github.com/tao-vin/opsera/internal/cli"
)

func main() {
	cli.SetRelayAgentRunner(app.RunRelayAgentForCLI)
	if handled, err := cli.TryRun(os.Args[1:]); handled {
		if err != nil {
			log.Print(err)
			os.Exit(1)
		}
		return
	}
	if relaunched, err := app.RelaunchGUIIfNeeded(); err != nil {
		log.Print(err)
	} else if relaunched {
		return
	}
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
