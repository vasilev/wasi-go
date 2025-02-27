package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/stealthrocket/wasi-go"
	"github.com/stealthrocket/wasi-go/imports"
	"github.com/stealthrocket/wasi-go/imports/wasi_http"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/sys"
)

func printUsage() {
	fmt.Printf(`wasirun - Run a WebAssembly module

USAGE:
   wasirun [OPTIONS]... <MODULE> [--] [ARGS]...

ARGS:
   <MODULE>
      The path of the WebAssembly module to run

   [ARGS]...
      Arguments to pass to the module

OPTIONS:
   --dir <DIR>
      Grant access to the specified host directory

   --listen <ADDR:PORT>
      Grant access to a socket listening on the specified address

   --dial <ADDR:PORT>
      Grant access to a socket connected to the specified address

   --dns-server <ADDR:PORT>
      Sets the address of the DNS server to use for name resolution

   --env-inherit
      Inherits all environment variables from the calling process

   --env <NAME=VAL>
      Pass an environment variable to the module. Overrides
      any inherited environment variables from --env-inherit

   --sockets <NAME>
      Enable a sockets extension, either {none, auto, path_open,
      wasmedgev1, wasmedgev2}

   --pprof-addr <ADDR:PORT>
      Start a pprof server listening on the specified address

   --trace
      Enable logging of system calls (like strace)

   --non-blocking-stdio
      Enable non-blocking stdio

   --http <MODE>
      Optionally enable wasi-http client support and select a
      version {none, auto, v1}

   -v, --version
      Print the version and exit

   -h, --help
      Show this usage information
`)
}

var (
	envInherit       bool
	envs             stringList
	dirs             stringList
	listens          stringList
	dials            stringList
	dnsServer        string
	socketExt        string
	pprofAddr        string
	wasiHttp         string
	trace            bool
	nonBlockingStdio bool
	version          bool
)

func main() {
	flagSet := flag.NewFlagSet("wasirun", flag.ExitOnError)
	flagSet.Usage = printUsage

	flagSet.BoolVar(&envInherit, "env-inherit", false, "")
	flagSet.Var(&envs, "env", "")
	flagSet.Var(&dirs, "dir", "")
	flagSet.Var(&listens, "listen", "")
	flagSet.Var(&dials, "dial", "")
	flagSet.StringVar(&dnsServer, "dns-server", "", "")
	flagSet.StringVar(&socketExt, "sockets", "auto", "")
	flagSet.StringVar(&pprofAddr, "pprof-addr", "", "")
	flagSet.StringVar(&wasiHttp, "http", "auto", "")
	flagSet.BoolVar(&trace, "trace", false, "")
	flagSet.BoolVar(&nonBlockingStdio, "non-blocking-stdio", false, "")
	flagSet.BoolVar(&version, "version", false, "")
	flagSet.BoolVar(&version, "v", false, "")
	flagSet.Parse(os.Args[1:])

	if version {
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "(devel)" {
			fmt.Println("wasirun", info.Main.Version)
		} else {
			fmt.Println("wasirun", "devel")
		}
		os.Exit(0)
	}

	args := flagSet.Args()
	if len(args) == 0 {
		printUsage()
		os.Exit(1)
	}

	if envInherit {
		envs = append(append([]string{}, os.Environ()...), envs...)
	}

	if dnsServer != "" {
		_, dnsServerPort, _ := net.SplitHostPort(dnsServer)
		net.DefaultResolver.PreferGo = true
		net.DefaultResolver.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			var d net.Dialer
			if dnsServerPort != "" {
				address = dnsServer
			} else {
				_, port, err := net.SplitHostPort(address)
				if err != nil {
					return nil, net.InvalidAddrError(address)
				}
				address = net.JoinHostPort(dnsServer, port)
			}
			return d.DialContext(ctx, network, address)
		}
	}

	if err := run(args[0], args[1:]); err != nil {
		if exitErr, ok := err.(*sys.ExitError); ok {
			os.Exit(int(exitErr.ExitCode()))
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(wasmFile string, args []string) error {
	wasmName := filepath.Base(wasmFile)
	wasmCode, err := os.ReadFile(wasmFile)
	if err != nil {
		return fmt.Errorf("could not read WASM file '%s': %w", wasmFile, err)
	}

	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}

	if pprofAddr != "" {
		go http.ListenAndServe(pprofAddr, nil)
	}

	ctx := context.Background()
	runtime := wazero.NewRuntime(ctx)
	defer runtime.Close(ctx)

	wasmModule, err := runtime.CompileModule(ctx, wasmCode)
	if err != nil {
		return err
	}
	defer wasmModule.Close(ctx)

	builder := imports.NewBuilder().
		WithName(wasmName).
		WithArgs(args...).
		WithEnv(envs...).
		WithDirs(dirs...).
		WithListens(listens...).
		WithDials(dials...).
		WithNonBlockingStdio(nonBlockingStdio).
		WithSocketsExtension(socketExt, wasmModule).
		WithTracer(trace, os.Stderr)

	var system wasi.System
	ctx, system, err = builder.Instantiate(ctx, runtime)
	if err != nil {
		return err
	}
	defer system.Close(ctx)

	importWasi := false
	switch wasiHttp {
	case "auto":
		importWasi = wasi_http.DetectWasiHttp(wasmModule)
	case "v1":
		importWasi = true
	case "none":
		importWasi = false
	default:
		return fmt.Errorf("invalid value for -http '%v', expected 'auto', 'v1' or 'none'", wasiHttp)
	}
	if importWasi {
		if err := wasi_http.Instantiate(ctx, runtime); err != nil {
			return err
		}
	}

	instance, err := runtime.InstantiateModule(ctx, wasmModule, wazero.NewModuleConfig())
	if err != nil {
		return err
	}
	return instance.Close(ctx)
}

type stringList []string

func (s stringList) String() string {
	return fmt.Sprintf("%v", []string(s))
}

func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}
