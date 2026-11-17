package runners

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mayusuk/agent-loop/internal/ports"
	"github.com/mayusuk/agent-loop/internal/secrets"
)

// CredentialHelperCommand is the agent-loop subcommand git runs as the
// run's credential helper: `agent-loop credential-helper <socket> <op>`.
const CredentialHelperCommand = "credential-helper"

// maxRequest caps one helper request.
const maxRequest = 64 << 10

// helperTimeout bounds one helper exchange on either side.
const helperTimeout = 30 * time.Second

// credServer serves git credentials for one run over a Unix socket
// (TECHNICAL-SPEC §7.3, work plan D13). It answers `get` with the push
// token for an https URL on the repository's host, and nothing otherwise:
// so a review run, which has no push token, gets nothing (INTEGRATION-SPEC
// §3.4).
type credServer struct {
	ln     net.Listener
	host   string // the repository's host, lower case; "" serves nothing
	push   ports.TokenSource
	redact *secrets.Redactor
	ctx    context.Context
	wg     sync.WaitGroup
}

// serveCredentials listens on socket (mode 0600) and serves until close.
func serveCredentials(ctx context.Context, socket, repoURL string, push ports.TokenSource, r *secrets.Redactor) (*credServer, error) {
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("credential helper: %w", err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("credential helper: %w", err)
	}
	s := &credServer{ln: ln, push: push, redact: r, ctx: ctx}
	if u, err := url.Parse(repoURL); err == nil && u.Scheme == "https" {
		s.host = strings.ToLower(u.Host)
	}
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

func (s *credServer) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer c.Close()
			c.SetDeadline(time.Now().Add(helperTimeout))
			s.handle(c)
		}()
	}
}

// close stops listening and waits for open exchanges to end.
func (s *credServer) close() {
	s.ln.Close()
	s.wg.Wait()
}

// handle reads one request, an operation line followed by git's
// credential attributes, and writes the answer.
func (s *credServer) handle(c net.Conn) {
	sc := bufio.NewScanner(io.LimitReader(c, maxRequest))
	if !sc.Scan() {
		return
	}
	op := strings.TrimSpace(sc.Text())
	attrs := map[string]string{}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			attrs[k] = v
		}
	}
	if op != "get" || s.push == nil || s.host == "" {
		return
	}
	if attrs["protocol"] != "https" || strings.ToLower(attrs["host"]) != s.host {
		return
	}
	tok, err := s.push.Token(s.ctx)
	if err != nil || tok.Secret() == "" || strings.ContainsAny(tok.Secret(), "\n\x00") {
		return
	}
	if s.redact != nil {
		s.redact.Add(tok.Secret(), tok.ExpiresAt)
	}
	fmt.Fprintf(c, "username=x-access-token\npassword=%s\n", tok.Secret())
}

// CredentialHelperMain is the client git runs as the credential helper:
// args are the socket and git's operation (get, store or erase). It
// forwards the operation and git's attributes from stdin to the run's
// helper socket and copies the answer to stdout. It returns the exit code.
func CredentialHelperMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintln(stderr, "usage: agent-loop credential-helper <socket> get|store|erase")
		return 2
	}
	socket, op := args[0], args[1]
	if op != "get" {
		// store and erase: the worker keeps no credentials from git.
		io.Copy(io.Discard, stdin)
		return 0
	}
	c, err := net.DialTimeout("unix", socket, helperTimeout)
	if err != nil {
		fmt.Fprintf(stderr, "agent-loop credential-helper: %v\n", err)
		return 1
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(helperTimeout))
	req, err := io.ReadAll(io.LimitReader(stdin, maxRequest))
	if err == nil {
		_, err = io.WriteString(c, op+"\n"+string(req))
	}
	if err == nil {
		err = c.(*net.UnixConn).CloseWrite()
	}
	if err == nil {
		_, err = io.Copy(stdout, c)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintf(stderr, "agent-loop credential-helper: %v\n", err)
		return 1
	}
	return 0
}
