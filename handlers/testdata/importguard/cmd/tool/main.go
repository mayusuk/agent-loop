// Command tool is a fixture: a core command may import the core and contract/.
package main

import (
	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
)

func main() { _, _ = contract.ValidateResult, core.Decide }
