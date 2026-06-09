// Command vsat-webapp serves the VSAT Cluster v2 web UI: a single-password gate
// over LXD container add/remove and an in-browser terminal into each container.
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/tall27/vsat-cluster-v2/internal/config"
	"github.com/tall27/vsat-cluster-v2/internal/httpserver"
	"github.com/tall27/vsat-cluster-v2/internal/lxdctl"
	"github.com/tall27/vsat-cluster-v2/internal/selfsign"
)

// version is set at build time via -ldflags "-X main.version=<git-sha>".
var version = "dev"

func main() {
	var (
		addr      = flag.String("addr", ":8443", "listen address")
		host      = flag.String("host", defaultHost(), "host label shown in the UI (public IP or DNS name)")
		lxcBin    = flag.String("lxc-bin", "lxc", "path to the lxc binary")
		sudo      = flag.Bool("sudo", false, "run lxc via 'sudo -n'")
		configDir = flag.String("config-dir", "", "config directory (default: OS user-config dir or $VSAT_CONFIG_DIR)")
		certFile  = flag.String("tls-cert", "", "TLS certificate file (PEM); if unset a self-signed cert is generated")
		keyFile   = flag.String("tls-key", "", "TLS key file (PEM)")
		httpOnly  = flag.Bool("http", false, "serve plain HTTP instead of HTTPS (lab/demo only)")
		maxCont   = flag.Int("max-containers", config.DefaultMaxContainers, "maximum number of containers")
		prefix    = flag.String("prefix", config.DefaultInstancePrefix, "required container-name prefix")
	)
	flag.Parse()

	logger := log.New(os.Stderr, "", log.LstdFlags)

	lxd := lxdctl.New(lxdctl.Options{
		Bin:    *lxcBin,
		Sudo:   *sudo,
		Prefix: *prefix,
		Max:    *maxCont,
	})

	srv, err := httpserver.New(httpserver.Options{
		Store:         config.NewStore(*configDir),
		LXD:           lxd,
		LXCBin:        *lxcBin,
		Sudo:          *sudo,
		Host:          *host,
		SecureCookies: !*httpOnly,
		Logger:        logger,
		Version:       version,
	})
	if err != nil {
		logger.Fatalf("init server: %v", err)
	}

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: the terminal WebSocket is long-lived.
	}

	if *httpOnly {
		logger.Printf("serving HTTP on %s (host=%s) — INSECURE, lab use only", *addr, *host)
		if err := httpSrv.ListenAndServe(); err != nil {
			logger.Fatalf("server: %v", err)
		}
		return
	}

	tlsConfig, err := buildTLS(*certFile, *keyFile, *host)
	if err != nil {
		logger.Fatalf("tls: %v", err)
	}
	httpSrv.TLSConfig = tlsConfig
	logger.Printf("serving HTTPS on %s (host=%s)", *addr, *host)
	if err := httpSrv.ListenAndServeTLS("", ""); err != nil {
		logger.Fatalf("server: %v", err)
	}
}

func buildTLS(certFile, keyFile, host string) (*tls.Config, error) {
	if certFile != "" && keyFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("load keypair: %w", err)
		}
		return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
	}
	cert, err := selfsign.Certificate(host)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

func defaultHost() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "localhost"
}
