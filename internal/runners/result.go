package runners

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/mayusuk/agent-loop/contract"
	"github.com/mayusuk/agent-loop/internal/ports"
)

// MaxResultFile is the largest result.json the runner reads (work plan D15).
const MaxResultFile = 1 << 20

// ReadResult reads the run's result.json and validates it against in
// (INTEGRATION-SPEC §3.1, §5). The exit code plays no part: a missing,
// unreadable, too large or invalid result is a failed result, whatever the
// handler's exit code (FR-18 AC3). The error says why, and is nil for a
// valid result. A run that reached its time limit is reported by the
// outcome's TimedOut, which the caller handles first (FR-18 AC4).
func ReadResult(in contract.Input, o ports.RunOutcome, outDir string) (contract.Result, error) {
	data, err := readLimited(o.ResultPath)
	if err != nil {
		err = fmt.Errorf("%w: result.json: %w", contract.ErrInvalidResult, err)
		return failed(in, err), err
	}
	return contract.ValidateResult(in, data, outDir)
}

func readLimited(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("no result path")
	}
	// Lstat first: a symlink, a FIFO or a directory is refused, and opening
	// a FIFO would block.
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("missing: the handler wrote no result")
	}
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxResultFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxResultFile {
		return nil, fmt.Errorf("larger than %d bytes", MaxResultFile)
	}
	return data, nil
}

// failed is the failed result that stands in for one that couldn't be read.
func failed(in contract.Input, err error) contract.Result {
	reason := strings.Join(strings.Fields(err.Error()), " ")
	if utf8.RuneCountInString(reason) > contract.MaxFailedReason {
		reason = string([]rune(reason)[:contract.MaxFailedReason-1]) + "…"
	}
	return contract.Result{
		ContractVersion: contract.Version,
		RunID:           in.RunID,
		Result:          contract.OutcomeFailed,
		Failed:          &contract.Failed{Reason: reason},
	}
}
