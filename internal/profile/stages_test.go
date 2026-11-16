package profile

import (
	"strings"
	"testing"
)

// TestD9_HandlerStageBoundaries is review R3: the time_limit and attempts
// boundaries for every handler stage, not just merge.
func TestD9_HandlerStageBoundaries(t *testing.T) {
	cases := []struct{ setting, key string }{
		{`time_limit = "1m"`, ""},
		{`time_limit = "24h"`, ""},
		{`time_limit = "59s"`, "time_limit"},
		{`time_limit = "25h"`, "time_limit"},
		{`attempts = 1`, ""},
		{`attempts = 10`, ""},
		{`attempts = 0`, "attempts"},
		{`attempts = 11`, "attempts"},
	}
	for _, stage := range []string{StageImplement, StageReview, StageAddress, StageMint} {
		for _, c := range cases {
			t.Run(stage+" "+c.setting, func(t *testing.T) {
				header := "[stages." + stage + "]\n"
				src := strings.Replace(minimalTables, header, header+c.setting+"\n", 1)
				if stage == StageMint {
					src = minimalTables + "\n" + header + "command = [\"h\"]\ncontract = \"1.0\"\n" + c.setting + "\n"
				}
				_, err := parseProfile(testFile, []byte(src), nil)
				if c.key == "" {
					if err != nil {
						t.Fatalf("rejected:\n%v", err)
					}
					return
				}
				wantProblem(t, err, testFile, "stages."+stage+"."+c.key)
			})
		}
	}
}
