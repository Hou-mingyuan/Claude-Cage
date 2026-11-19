// Deployment candidate for the Windows remote AppContainer network adapter.
// The workload cannot access the admin-only gate control socket or config.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/guestos"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if runtime.GOOS != "windows" || len(os.Args) != 4 {
		return errors.New("usage: remote-egress serve|check|close|status CONFIG CREDENTIAL_FILE")
	}
	action, configPath, credentials := os.Args[1], os.Args[2], os.Args[3]
	binding, err := guestos.LoadWindowsProfile(filepath.Dir(configPath))
	if err != nil {
		return err
	}
	if binding == nil || !binding.Remote() {
		return errors.New("remote Windows binding required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if action != "serve" {
		client, err := cage.OpenGateClient(binding.InstanceDirectory)
		if err != nil {
			return err
		}
		ctx, done := context.WithTimeout(ctx, 10*time.Second)
		defer done()
		var state cage.GateStatus
		switch action {
		case "check":
			state, err = client.BeginCheck(ctx)
		case "close":
			state, err = client.Close(ctx, "remote deployment check finished")
		case "status":
			state, err = client.Status(ctx)
		default:
			return errors.New("invalid action")
		}
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(state)
	}
	cfg, err := cage.Load(configPath)
	if err != nil {
		return err
	}
	if err = cfg.RequireManagedProxy(filepath.Dir(configPath)); err != nil {
		return err
	}
	data, err := os.ReadFile(credentials)
	if err != nil {
		return err
	}
	var credential struct {
		Token string `json:"token"`
	}
	if err = json.Unmarshal(data, &credential); err != nil {
		return errors.New("invalid broker credentials")
	}
	gate, err := cage.NewEgressGate(cfg)
	if err != nil {
		return err
	}
	defer gate.Close("remote broker stopped")
	handler, err := cage.GateProxy(gate, credential.Token)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:17897")
	if err != nil {
		return err
	}
	defer listener.Close()
	stop, err := cage.StartGateControl(configPath, binding.InstanceDirectory, gate)
	if err != nil {
		return err
	}
	defer stop()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10}
	go func() { <-ctx.Done(); gate.Close("remote broker stopped"); _ = server.Close() }()
	fmt.Println(`{"broker":"127.0.0.1:17897","mode":"closed","workload_ready":false}`)
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
