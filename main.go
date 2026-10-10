package main

import (
	"fmt"
	"os"
)

func main() {
	clients, err := newClients()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	app := newApp(clients, loadConfig())
	if err := app.run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
