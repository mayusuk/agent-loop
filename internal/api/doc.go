// Package api is the local gRPC service the CLI talks to, served on a Unix
// socket only the owner can open, with an optional HTTP/JSON gateway that is
// off by default (TECHNICAL-SPEC §11, T13).
package api
