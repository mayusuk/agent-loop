package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	apiv1 "github.com/mayusuk/agent-loop/internal/api/v1"
)

// sunPathSize is the size of sun_path in struct sockaddr_un: 108 bytes on
// Linux, 104 on macOS and the other BSDs.
func sunPathSize() int {
	if runtime.GOOS == "linux" {
		return 108
	}
	return 104
}

// CheckSocketPath returns an error naming path, its length and the limit
// when path doesn't fit a Unix socket address with its terminating NUL
// (work plan #139 D1, #174 D1).
func CheckSocketPath(path string) error {
	if n := sunPathSize(); len(path) >= n {
		return fmt.Errorf("the API socket path %s is %d bytes, too long for a Unix socket (at most %d here)", path, len(path), n-1)
	}
	return nil
}

// Listen listens on the Unix socket at path (TECHNICAL-SPEC §11): its
// directory is created, or narrowed, to mode 0700 and the socket is 0600,
// so only the owner's account can open it. A path too long for a Unix
// socket is refused, naming it and the limit (#174 D1). A stale socket left by a server
// that is gone is replaced; one a running server answers on is refused.
func Listen(path string) (net.Listener, error) {
	if err := CheckSocketPath(path); err != nil {
		return nil, fmt.Errorf("api: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("api: socket directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("api: socket directory: %w", err)
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("api: %s exists and isn't a socket", path)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("api: a server is already listening on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("api: removing the stale socket: %w", err)
		}
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("api: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, fmt.Errorf("api: socket mode: %w", err)
	}
	return l, nil
}

// NewServer returns a gRPC server serving s.
func NewServer(s apiv1.AgentLoopServer) *grpc.Server {
	g := grpc.NewServer()
	apiv1.RegisterAgentLoopServer(g, s)
	return g
}

// Dial returns a client of the server on the Unix socket at path. The
// connection is made on the first call, so a path too long for a Unix socket
// is refused here, not with a bare "invalid argument" then (#174 D1).
func Dial(path string) (apiv1.AgentLoopClient, io.Closer, error) {
	if err := CheckSocketPath(path); err != nil {
		return nil, nil, fmt.Errorf("api: %w", err)
	}
	conn, err := grpc.NewClient("unix://"+path, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, fmt.Errorf("api: %w", err)
	}
	return apiv1.NewAgentLoopClient(conn), conn, nil
}

// Gateway is the optional HTTP/JSON gateway (TECHNICAL-SPEC §11): each
// method is POST /v1/<Method> with the request as protobuf JSON, and needs
// "Authorization: Bearer <token>". The deployment config keeps it off by
// default and on a loopback address (work plan #22 D2).
func Gateway(s apiv1.AgentLoopServer, token string) http.Handler {
	type method struct {
		req  func() proto.Message
		call func(ctx context.Context, m proto.Message) (proto.Message, error)
	}
	methods := map[string]method{
		"Board": {func() proto.Message { return &apiv1.BoardRequest{} }, func(ctx context.Context, m proto.Message) (proto.Message, error) {
			return s.Board(ctx, m.(*apiv1.BoardRequest))
		}},
		"ItemCommand": {func() proto.Message { return &apiv1.ItemCommandRequest{} }, func(ctx context.Context, m proto.Message) (proto.Message, error) {
			return s.ItemCommand(ctx, m.(*apiv1.ItemCommandRequest))
		}},
		"ProjectCommand": {func() proto.Message { return &apiv1.ProjectCommandRequest{} }, func(ctx context.Context, m proto.Message) (proto.Message, error) {
			return s.ProjectCommand(ctx, m.(*apiv1.ProjectCommandRequest))
		}},
		"DeploymentCommand": {func() proto.Message { return &apiv1.DeploymentCommandRequest{} }, func(ctx context.Context, m proto.Message) (proto.Message, error) {
			return s.DeploymentCommand(ctx, m.(*apiv1.DeploymentCommandRequest))
		}},
		"ItemLog": {func() proto.Message { return &apiv1.ItemLogRequest{} }, func(ctx context.Context, m proto.Message) (proto.Message, error) {
			return s.ItemLog(ctx, m.(*apiv1.ItemLogRequest))
		}},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" || !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		name, ok := strings.CutPrefix(r.URL.Path, "/v1/")
		m, known := methods[name]
		if !ok || !known {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req := m.req()
		if len(body) > 0 {
			if err := protojson.Unmarshal(body, req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		res, err := m.call(r.Context(), req)
		if err != nil {
			st, _ := status.FromError(err)
			http.Error(w, st.Message(), httpStatus(st.Code()))
			return
		}
		out, err := protojson.Marshal(res)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(out)
	})
}

func httpStatus(c codes.Code) int {
	switch c {
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.NotFound:
		return http.StatusNotFound
	case codes.FailedPrecondition:
		return http.StatusConflict
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// ErrGatewayAddress is returned by ListenGateway for an address that isn't
// loopback.
var ErrGatewayAddress = errors.New("api: the gateway listens only on a loopback address")

// ListenGateway listens on addr, which must be a loopback address
// (TECHNICAL-SPEC §11).
func ListenGateway(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("api: gateway address: %w", err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("%w: %s", ErrGatewayAddress, addr)
	}
	return net.Listen("tcp", addr)
}
