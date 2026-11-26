// Package bad is a fixture: a handler package that imports the core.
package bad

import (
	"fmt"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/core"
)

var _, _, _ = fmt.Sprint, contract.ValidateResult, core.Decide
