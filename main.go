package main

// github-core-clone-01: a standalone GitHub-compatible git forge.
//
// Lifecycle: bind an ephemeral port, POST {"port":N} to $MAKEFUN_ENV_URL/register,
// long-poll $MAKEFUN_ENV_URL/done until it answers {"done":true}, then exit 0.

import (
	"fmt"
	"net"
	"os"
	"strconv"
)

func main() {
	store := newStore()
	registerCore()
	registerGit()
	registerRest()
	registerGraphql()
	registerMcp()

	bind := "0.0.0.0:0"
	if port := os.Getenv("PORT"); port != "" {
		bind = "0.0.0.0:" + port
	}
	listener, err := net.Listen("tcp", bind)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen failed: %v\n", err)
		os.Exit(1)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	server := newForgeServer(newForge(store))
	go func() {
		if err := server.serve(listener); err != nil {
			fmt.Fprintf(os.Stderr, "[forge] serve stopped: %v\n", err)
		}
	}()

	envURL := os.Getenv("MAKEFUN_ENV_URL")
	fmt.Fprintf(os.Stderr, "[forge] listening on :%d\n", port)
	if envURL == "" {
		select {}
	}

	if err := register(envURL, port); err != nil {
		fmt.Fprintf(os.Stderr, "[forge] register failed: %v\n", err)
		os.Exit(1)
	}
	waitForDone(envURL)
	os.Exit(0)
}

var _ = strconv.Itoa
