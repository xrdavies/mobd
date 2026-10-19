package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/xrdavies/mobd/internal/api"
	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/control"
	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/mcpserver"
	"github.com/xrdavies/mobd/internal/pairing"
	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/runner/claude"
	"github.com/xrdavies/mobd/internal/runner/codex"
	"github.com/xrdavies/mobd/internal/service"
	"github.com/xrdavies/mobd/internal/task"
)

// version is set from the release tag with -ldflags.
var version = "0.3.0"

const usage = `Usage: mobd <command> [options]
  init       Create ~/.mobd/config.json (never overwrite)
  serve      Run in the foreground (also the default command)
  start      Start the background service (LaunchAgent / systemd user unit); --login starts it at login or boot
  stop       Stop the background service and disable start at login
  restart    Reload configuration and restart; preserve login setting
  status     Show background service address and health
  logs       Follow background service logs
  link       --url HTTP(S)_ORIGIN: print a one-use pairing link and QR; --close: close invitations
  clients    list | revoke CLIENT_ID | delete CLIENT_ID (local management)
  connections  List authenticated active requests and streams
  auth       reset --yes: invalidate all clients and invitations
  mcp        Run the built-in MCP server over stdio
  --version  Print version
  --help     Show help

serve/start/restart options:
  --config PATH       JSON config (default ~/.mobd/config.json)
  --listen HOST:PORT  HTTP bind address
  --codex-bin PATH    Codex executable
  --workdir PATH      Codex working directory
  --state-path PATH   Conversation snapshot
  --control-socket PATH  Local management socket (default ~/.mobd/run/control.sock)
  --max-running N    Concurrent tasks (default 1)
Configuration: flags > exported MOBD_* environment > JSON > defaults.
Management commands accept --socket PATH or MOBD_CONTROL_SOCKET.
No .env file is read. Background commands operate on one service per user.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") || command == "help" {
		fmt.Print(usage)
		return nil
	}
	if len(args) == 1 && args[0] == "--version" || command == "version" {
		fmt.Println("mobd", version)
		return nil
	}
	if command == "link" || command == "clients" || command == "connections" || command == "auth" {
		return manage(command, args)
	}
	if command == "mcp" {
		if len(args) != 0 {
			return errors.New("usage: mobd mcp")
		}
		log.SetOutput(os.Stderr)
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return mcpserver.Run(ctx, version)
	}
	if command == "init" {
		fs := flag.NewFlagSet("init", flag.ContinueOnError)
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}
		fs.StringVar(&path, "config", path, "JSON config path")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("usage: mobd init [--config PATH]")
		}
		if err := config.Init(path); err != nil {
			return err
		}
		fmt.Println("Created", path)
		return nil
	}
	if command == "status" || command == "logs" || command == "stop" {
		if len(args) != 0 {
			return fmt.Errorf("%s does not take options", command)
		}
		manager, err := service.New()
		if err != nil {
			return err
		}
		switch command {
		case "status":
			return manager.Status()
		case "logs":
			return manager.Logs()
		case "stop":
			if err := manager.Stop(); err != nil {
				return err
			}
			fmt.Println("mobd stopped; login startup disabled")
			return nil
		}
	}
	if command != "serve" && command != "start" && command != "restart" {
		return fmt.Errorf("unknown command %q\n%s", command, usage)
	}
	login := false
	if command != "serve" {
		filtered := make([]string, 0, len(args))
		for _, arg := range args {
			if arg == "--login" {
				login = true
			} else {
				filtered = append(filtered, arg)
			}
		}
		args = filtered
	}
	if command == "restart" && len(args) == 0 {
		manager, err := service.New()
		if err != nil {
			return err
		}
		args, err = manager.SavedArgs()
		if err != nil {
			return err
		}
	}
	cfg, path, err := config.Load(args)
	if err != nil {
		return err
	}
	if command != "serve" {
		manager, err := service.New()
		if err != nil {
			return err
		}
		if command == "restart" {
			login = login || manager.LoginEnabled()
			if err := manager.Stop(); err != nil {
				return err
			}
		}
		if err := manager.Start(cfg, args, login); err != nil {
			return err
		}
		fmt.Printf("mobd started; listen=%s; health=%s/health\nconfig=%s; logs=%s\n", cfg.ListenAddr, service.LocalURL(cfg.ListenAddr), path, manager.LogPath())
		return nil
	}
	return serve(cfg)
}

func streamPort(cfg config.Config) string {
	if cfg.StreamPort == 0 {
		return "random"
	}
	return strconv.Itoa(cfg.StreamPort)
}

func serve(cfg config.Config) error {
	// Ask for missing macOS TCC grants before starting the remote API.
	desktop.RequestPermissions()

	// Bind before opening state so a duplicate server cannot modify the snapshot.
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	defer listener.Close()
	if cfg.StreamPort > 0 {
		udp, err := net.ListenUDP("udp", &net.UDPAddr{Port: cfg.StreamPort})
		if err != nil {
			return fmt.Errorf("live desktop UDP port %d: %w", cfg.StreamPort, err)
		}
		defer udp.Close()
		cfg.StreamUDP = udp
	}

	management, release, err := control.Listen(cfg.ControlSocket)
	if err != nil {
		return err
	}
	defer release()
	auth, err := pairing.Open(filepath.Join(cfg.DataDir, "auth.json"))
	if err != nil {
		return err
	}
	defer auth.Close()

	store, err := task.OpenStore(cfg.StatePath)
	if err != nil {
		return fmt.Errorf("open conversation state: %w", err)
	}
	available := map[string]runner.Runner{codex.Name: codex.New(cfg), claude.Name: claude.New(cfg)}
	var runners []runner.Runner
	for _, name := range cfg.Runners {
		rn, ok := available[name]
		if !ok {
			return fmt.Errorf("unknown runner %q (known: codex, claude)", name)
		}
		runners = append(runners, rn)
	}
	if _, err := runner.NewRegistry(cfg.DefaultRunner, runners...); err != nil {
		return fmt.Errorf("defaultRunner: %w", err)
	}
	desktops := newProvider(cfg)
	if desktops != nil {
		// Desktops outlive mobd; the next one finds them.
		defer desktops.Shutdown()
	}
	srv := api.New(cfg, store, auth, desktops, runners...)

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	defer httpSrv.Close()

	adminSrv := &http.Server{Handler: control.Handler(auth), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	defer adminSrv.Close()
	serveErr := make(chan error, 2)
	go func() { serveErr <- adminSrv.Serve(management) }()
	go func() {
		log.Printf("mobd listening on %s, live desktop UDP %s; health=%s/health (runners=%s default=%s)", cfg.ListenAddr, streamPort(cfg), cfg.SelfURL(), strings.Join(cfg.Runners, ","), cfg.DefaultRunner)
		serveErr <- httpSrv.Serve(listener)
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			srv.Close()
			return err
		}
	}
	srv.Close()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	log.Printf("mobd stopped")
	return nil
}
