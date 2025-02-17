// Tool gokr-rsync is an rsync receiver Go implementation.
package main

import (
	"log"
	"os"

	"github.com/gokrazy/rsync/internal/receivermaincmd"
)

func main() {
	if _, err := receivermaincmd.ClientMain(os.Args, os.Stdin, os.Stdout, os.Stderr); err != nil {
		log.Fatal(err)
	}
}
