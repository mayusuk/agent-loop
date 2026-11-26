// Package good is a fixture: the imports D34 allows a handler.
package good

import (
	"os"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/handlers/internal/run"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

var _, _, _, _ = os.Exit, contract.ValidateResult, run.Contracts, jsonschema.NewCompiler
