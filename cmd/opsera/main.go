package main

import (
	"log"
	"os"

	"github.com/tao-vin/opsera/internal/app"
	"github.com/tao-vin/opsera/internal/cli"
)

func main() {
	if handled, err := cli.TryRun(os.Args[1:]); handled {
		if err != nil {
			os.Exit(1)
		}
		return
	}
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
