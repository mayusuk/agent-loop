package main

import (
	"fmt"
	"io"
)

func runServerStart(args []string, stdout, stderr io.Writer) int {
	fmt.Fprintln(stderr, "agent-loop server start: not implemented yet")
	return 1
}
