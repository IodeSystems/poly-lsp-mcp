package main

import (
	"log"
	"os"

	"github.com/iodesystems/tslsmcp/internal/server"
)

func main() {
	log.SetOutput(os.Stderr)
	log.SetPrefix("tslsmcp ")
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	srv := server.New()
	if err := srv.Serve(os.Stdin, os.Stdout); err != nil {
		log.Fatal(err)
	}
}
