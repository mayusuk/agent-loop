package contract

import (
	"go/build"
	"testing"
)

// TestContractImportsOnlyStdlib keeps package contract on the standard
// library, so the core and the handlers, which import it, do too. Code that
// needs other modules goes in a subpackage, such as contract/tasklist (work
// plan #154 D1).
func TestContractImportsOnlyStdlib(t *testing.T) {
	pkg, err := build.Default.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range pkg.Imports {
		p, err := build.Default.Import(imp, "", build.FindOnly)
		if err != nil || !p.Goroot {
			t.Errorf("package contract imports %s, which is not in the standard library", imp)
		}
	}
}
