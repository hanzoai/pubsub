// Copyright 2012-2026 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

//go:generate go run server/errors_gen.go

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/hanzoai/pubsub/internal/consensus"
	"github.com/hanzoai/pubsub/internal/mgmt"
	"github.com/hanzoai/pubsub/internal/store"
	"github.com/hanzoai/pubsub/server"
)

var usageStr = `
Usage: pubsub [options]

Server Options:
    -a, --addr, --net <host>         Bind to host address (default: 0.0.0.0)
    -p, --port <port>                Use port for clients (default: 4222)
    -n, --name
        --server_name <server_name>  Server name (default: auto)
    -P, --pid <file>                 File to store PID
    -m, --http_port <port>           Use port for http monitoring
    -ms,--https_port <port>          Use port for https monitoring
    -c, --config <file>              Configuration file
    -t                               Test configuration and exit
    -sl,--signal <signal>[=<pid>]    Send signal to pubsub process (ldm, stop, quit, term, reopen, reload)
                                     <pid> can be either a PID (e.g. 1) or the path to a PID file (e.g. /var/run/pubsub.pid)
        --client_advertise <string>  Client URL to advertise to other servers
        --ports_file_dir <dir>       Creates a ports file in the specified directory (<executable_name>_<pid>.ports).

Logging Options:
    -l, --log <file>                 File to redirect log output
    -T, --logtime                    Timestamp log entries (default: true)
    -s, --syslog                     Log to syslog or windows event log
    -r, --remote_syslog <addr>       Syslog server addr (udp://localhost:514)
    -D, --debug                      Enable debugging output
    -V, --trace                      Trace the raw protocol
    -VV                              Verbose trace (traces system account as well)
    -DV                              Debug and trace
    -DVV                             Debug and verbose trace (traces system account as well)
        --log_size_limit <limit>     Logfile size limit (default: auto)
        --max_traced_msg_len <len>   Maximum printable length for traced messages (default: unlimited)

PubSub Options:
    -js, --jetstream                 Enable PubSub persistence (streams, consumers, KV)
    -sd, --store_dir <dir>           Set the storage directory

Authorization Options:
        --user <user>                User required for connections
        --pass <password>            Password required for connections
        --auth <token>               Authorization token required for connections

TLS Options:
        --tls                        Enable TLS, do not verify clients (default: false)
        --tlscert <file>             Server certificate file
        --tlskey <file>              Private key for server certificate
        --tlsverify                  Enable TLS, verify client certificates
        --tlscacert <file>           Client certificate CA for verification

Cluster Options:
        --routes <rurl-1, rurl-2>    Routes to solicit and connect
        --cluster <cluster-url>      Cluster URL for solicited routes
        --cluster_name <string>      Cluster Name, if not set one will be dynamically generated
        --no_advertise <bool>        Do not advertise known cluster information to clients
        --cluster_advertise <string> Cluster URL to advertise to other servers
        --connect_retries <number>   For implicit routes, number of connect retries
        --cluster_listen <url>       Cluster url from which members can solicit routes

Management Options:
    PUBSUB_ZAP_PORT                  ZAP control plane port (default: 9222, env)
    PUBSUB_HTTP_PORT                 HTTP management API port (default: 9280, env)

Quasar PQ Consensus (env):
    PUBSUB_QUASAR_ENABLED            Enable Quasar PQ consensus (default: false)
    PUBSUB_QUASAR_THRESHOLD           Signature threshold (default: 1)
    PUBSUB_QUASAR_VALIDATORS          Comma-separated validator IDs

Store (env):
    PUBSUB_STORE_DIR                  zapdb data directory (default: <store_dir>/zapdb)
    PUBSUB_STORE_ENABLED              Enable zapdb store (default: false)

Profiling Options:
        --profile <port>             Profiling HTTP port

Common Options:
    -h, --help                       Show this message
    -v, --version                    Show version
        --help_tls                   TLS help
`

// usage will print out the flag options for the server.
func usage() {
	fmt.Printf("%s\n", usageStr)
	os.Exit(0)
}

func main() {
	exe := "pubsub"

	// Create a FlagSet and sets the usage
	fs := flag.NewFlagSet(exe, flag.ExitOnError)
	fs.Usage = usage

	// Configure the options from the flags/config file
	opts, err := server.ConfigureOptions(fs, os.Args[1:],
		server.PrintServerAndExit,
		fs.Usage,
		server.PrintTLSHelpAndDie)
	if err != nil {
		server.PrintAndDie(fmt.Sprintf("%s: %s", exe, err))
	} else if opts.CheckConfig {
		fmt.Fprintf(os.Stderr, "%s: configuration file %s is valid (%s)\n", exe, opts.ConfigFile, opts.ConfigDigest())
		os.Exit(0)
	}

	// PubSub: JetStream enabled by default (streams, consumers, KV, Quasar consensus).
	// Can be explicitly disabled with --no-jetstream or config file.
	if !opts.JetStream {
		opts.JetStream = true
	}

	// Create the server with appropriate options.
	s, err := server.NewServer(opts)
	if err != nil {
		server.PrintAndDie(fmt.Sprintf("%s: %s", exe, err))
	}

	// Configure the logger based on the flags.
	s.ConfigureLogger()

	// Start things up. Block here until done.
	if err := server.Run(s); err != nil {
		server.PrintAndDie(err.Error())
	}

	// Start management server (ZAP transport + HTTP routes)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	mgmtCfg := mgmt.Config{
		NATSServer: s,
		Logger:     logger,
		HTTPToken:  os.Getenv("PUBSUB_HTTP_TOKEN"),
		ZAPSecret:  []byte(os.Getenv("PUBSUB_ZAP_SECRET")),
	}
	// Clear empty ZAP secret so auth is disabled in dev mode
	if len(mgmtCfg.ZAPSecret) == 0 {
		mgmtCfg.ZAPSecret = nil
	}

	// Quasar PQ consensus (opt-in via env)
	if os.Getenv("PUBSUB_QUASAR_ENABLED") == "true" || os.Getenv("PUBSUB_QUASAR_ENABLED") == "1" {
		threshold := envIntDefault("PUBSUB_QUASAR_THRESHOLD", 1)
		var validators []string
		if v := os.Getenv("PUBSUB_QUASAR_VALIDATORS"); v != "" {
			for _, id := range strings.Split(v, ",") {
				if id = strings.TrimSpace(id); id != "" {
					validators = append(validators, id)
				}
			}
		}
		var zapPeers []string
		if v := os.Getenv("PUBSUB_QUASAR_PEERS"); v != "" {
			for _, addr := range strings.Split(v, ",") {
				if addr = strings.TrimSpace(addr); addr != "" {
					zapPeers = append(zapPeers, addr)
				}
			}
		}
		mgmtCfg.Quasar = &consensus.Config{
			Threshold:    threshold,
			ValidatorIDs: validators,
			ValidatorID:  os.Getenv("PUBSUB_QUASAR_VALIDATOR_ID"),
			ZAPPort:      envIntDefault("PUBSUB_QUASAR_ZAP_PORT", 9223),
			ZAPPeers:     zapPeers,
			ZAPDiscover:  os.Getenv("PUBSUB_QUASAR_DISCOVER") == "true",
		}
		logger.Info("quasar PQ consensus enabled",
			"threshold", threshold,
			"validators", len(validators),
			"peers", len(zapPeers))
	}

	// zapdb store (opt-in via env)
	if os.Getenv("PUBSUB_STORE_ENABLED") == "true" || os.Getenv("PUBSUB_STORE_ENABLED") == "1" {
		dir := os.Getenv("PUBSUB_STORE_DIR")
		if dir == "" && opts.StoreDir != "" {
			dir = filepath.Join(opts.StoreDir, "zapdb")
		}
		if dir == "" {
			dir = filepath.Join(os.TempDir(), "pubsub-zapdb")
		}
		mgmtCfg.Store = &store.Config{Dir: dir, SyncWrites: true}
		logger.Info("zapdb store enabled", "dir", dir)
	}

	ms := mgmt.New(mgmtCfg)
	if err := ms.Start(); err != nil {
		// Log but don't die -- NATS core is already running
		logger.Error("failed to start management server", "error", err)
	} else {
		defer ms.Stop()
	}

	s.WaitForShutdown()
}

func envIntDefault(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return n
}
