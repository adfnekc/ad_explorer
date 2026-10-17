// Command ad_explorer is the Web gateway for RDP sessions.
//
// It terminates an authenticated WebSocket from a browser and drives a grdp
// client on the other side. Host addresses and passwords live in the browser:
// this process sees a password once, for the length of one session, and never
// writes it anywhere.
package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/adfnekc/grdp/glog"

	"github.com/adfnekc/ad_explorer/internal/auth"
	"github.com/adfnekc/ad_explorer/internal/httpx"
	"github.com/adfnekc/ad_explorer/internal/session"
	"github.com/adfnekc/ad_explorer/internal/webui"
)

// version is stamped at build time.
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ad_explorer: "+err.Error())
		os.Exit(1)
	}
}

type options struct {
	listen          string
	password        string
	passwordFile    string
	fromFlag        bool
	sessionTTL      time.Duration
	sessionSecret   string
	maxSessions     int
	idleTimeout     time.Duration
	dialTimeout     time.Duration
	maxFPS          int
	forceFullFrames bool
	reconnectGrace  time.Duration
	secureCookie    bool
	tlsCert         string
	tlsKey          string
	trustProxy      bool
	debug           bool
	traceInput      bool
	demo            bool
	showVersion     bool
}

func run(args []string) error {
	opts, err := parseFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if opts.showVersion {
		fmt.Println("ad_explorer " + version)
		return nil
	}

	logger := log.New(os.Stderr, "", log.LstdFlags|log.LUTC)

	// Pin grdp's logger before anything can connect. It writes the account
	// password at INFO level during the CredSSP handshake
	// (protocol/nla/ntlm.go:442), so the default here is WARN and only
	// -debug may raise it.
	switch {
	case opts.traceInput:
		// TRACE prints the bytes of every PDU grdp writes, which is the only
		// way to tell "the remote ignored the input" from "the input was never
		// sent". It also prints every received frame, so it is meant for a
		// short session, not for running the gateway.
		glog.SetLevel(glog.TRACE)
		logger.Print("WARNING: -trace-input raises grdp's log level to TRACE. " +
			"This prints the remote account password in plaintext during NLA " +
			"authentication, and the bytes of every frame. Use it for a short " +
			"session and then stop the gateway")
	case opts.debug:
		glog.SetLevel(glog.INFO)
		logger.Print("WARNING: -debug raises grdp's log level to INFO, which prints " +
			"the remote account password in plaintext during NLA authentication")
	default:
		glog.SetLevel(glog.WARN)
	}

	password, err := resolvePassword(opts)
	if err != nil {
		return err
	}
	if (opts.tlsCert == "") != (opts.tlsKey == "") {
		return errors.New("-tls-cert and -tls-key go together")
	}
	tlsOn := opts.tlsCert != ""
	if tlsOn && !opts.secureCookie {
		logger.Print("WARNING: -secure-cookie=false with TLS. The cookie would be " +
			"sent over the encrypted connection but readable by script, which is " +
			"the one thing it exists to prevent.")
	}

	if opts.fromFlag {
		logger.Print("WARNING: the console password was given on the command line, " +
			"where every user on this machine can read it from ps(1) and " +
			"/proc/<pid>/cmdline. Use -password-file with a 0600 file instead.")
	}

	secret, err := resolveSecret(opts)
	if err != nil {
		return err
	}

	authenticator, err := auth.New(auth.Options{
		Secret:   secret,
		Password: password,
		TTL:      opts.sessionTTL,
	})
	if err != nil {
		return err
	}

	if opts.maxFPS < 1 {
		return fmt.Errorf("-max-fps must be at least 1, not %d", opts.maxFPS)
	}
	// Capped because past this there is nothing left to gain: a desktop is shown
	// on a display that refreshes sixty or a hundred and twenty times a second,
	// so refreshes beyond that are spent encoding pictures that are replaced
	// before anything can see them. The limit is on how often the desktop is
	// redrawn, and one refresh is several messages when it is sent as strips.
	if opts.maxFPS > 120 {
		return fmt.Errorf("-max-fps must be at most 120, not %d", opts.maxFPS)
	}
	registry := session.NewRegistry(session.Options{
		Max:             opts.maxSessions,
		IdleTimeout:     opts.idleTimeout,
		FrameInterval:   time.Second / time.Duration(opts.maxFPS),
		ForceFullFrames: opts.forceFullFrames,
		ReconnectGrace:  opts.reconnectGrace,
		Demo:            opts.demo,
	})

	assets, err := webui.Assets()
	if err != nil {
		return fmt.Errorf("frontend assets: %w", err)
	}

	handler, err := httpx.New(httpx.Config{
		Auth:        authenticator,
		Sessions:    registry,
		Assets:      assets,
		Secure:      opts.secureCookie,
		TrustProxy:  opts.trustProxy,
		DialTimeout: opts.dialTimeout,
		TraceInput:  opts.traceInput,
		Logf:        logger.Printf,
	})
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", opts.listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", opts.listen, err)
	}

	srv := &http.Server{
		Handler: handler,
		// No ReadTimeout or WriteTimeout: a WebSocket is hijacked and then
		// long-lived, and a deadline left on the connection would cut it.
		// ReadHeaderTimeout still bounds a slowloris, and IdleTimeout bounds a
		// connection that is opened and then says nothing.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          logger,
	}

	address := listener.Addr().String()
	scheme := "http"
	if tlsOn {
		scheme = "https"
	}
	logger.Printf("ad_explorer %s listening on %s (%s)", version, address, scheme)
	if opts.demo {
		logger.Print("WARNING: -demo is on. Every session draws a synthetic desktop and " +
			"no remote host is contacted. Do not run this way in production.")
	}
	if !tlsOn {
		logger.Print("WARNING: serving plain HTTP. A remote password travels in a " +
			"request body, so this is only safe on a loopback interface or behind " +
			"something that terminates TLS. Pass -tls-cert and -tls-key to serve " +
			"HTTPS here instead.")
	}
	logger.Printf("open %s://%s/ and sign in with the console password", scheme, address)

	return serve(srv, listener, registry, logger, serveOptions{cert: opts.tlsCert, key: opts.tlsKey})
}

// serveOptions carries what the listener needs beyond the server itself.
type serveOptions struct {
	cert, key string
}

// serve runs the server until a signal arrives, then closes sessions first so
// each client is told why rather than just losing its socket.
func serve(srv *http.Server, listener net.Listener, registry *session.Registry, logger *log.Logger, opts serveOptions) error {
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	failed := make(chan error, 1)
	go func() {
		var err error
		if opts.cert != "" {
			err = srv.ServeTLS(listener, opts.cert, opts.key)
		} else {
			err = srv.Serve(listener)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			failed <- err
		}
	}()

	select {
	case err := <-failed:
		return err
	case sig := <-shutdown:
		logger.Printf("received %s, shutting down", sig)
	}

	registry.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	logger.Print("stopped")
	return nil
}

// ---------------------------------------------------------------- configuration

func parseFlags(args []string) (options, error) {
	var opts options

	fs := flag.NewFlagSet("ad_explorer", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "ad_explorer %s — a Web gateway for RDP sessions\n\n", version)
		fmt.Fprintf(fs.Output(), "Usage: ad_explorer [flags]\n\n")
		fs.PrintDefaults()
	}

	fs.StringVar(&opts.listen, "listen", envOr("ADX_LISTEN", "127.0.0.1:8443"),
		"address to listen on")
	fs.StringVar(&opts.password, "password", "",
		"console password. Visible to every user on this machine through ps(1); "+
			"prefer -password-file or ADX_PASSWORD")
	fs.StringVar(&opts.passwordFile, "password-file", "",
		"file holding the console password, which must be mode 0600")
	fs.DurationVar(&opts.sessionTTL, "session-ttl", auth.DefaultTTL,
		"how long a console sign-in stays valid")
	fs.StringVar(&opts.sessionSecret, "session-secret", "",
		"key that signs session cookies. Empty generates a random one, which logs "+
			"everyone out on restart; set it to share sessions across instances")
	fs.IntVar(&opts.maxSessions, "max-sessions", session.DefaultMaxSessions,
		"how many remote desktops may be open at once")
	fs.DurationVar(&opts.idleTimeout, "idle-timeout", session.DefaultIdleTimeout,
		"end a session after this long with no input. Negative disables it")
	fs.DurationVar(&opts.dialTimeout, "dial-timeout", httpx.DefaultDialTimeout,
		"how long a connection attempt may take")
	fs.DurationVar(&opts.reconnectGrace, "reconnect-grace", session.DefaultReconnectGrace,
		"how long a session whose browser went away is kept, so a reload or a network "+
			"blip can attach to the same desktop. Negative ends the session as soon as "+
			"the browser does")
	fs.BoolVar(&opts.forceFullFrames, "force-full-frames", false,
		"send every redraw as a whole desktop instead of the regions that changed: "+
			"a diagnostic for telling a fault in stitching changed regions apart "+
			"from one in showing a finished screen")
	fs.IntVar(&opts.maxFPS, "max-fps", 60,
		"the most times a second one session will redraw the desktop, up to 120. "+
			"Encoding is the expensive part of a session, so a server that draws in a "+
			"burst has its regions merged into fewer redraws rather than encoded one "+
			"per update, and one redraw is several messages when it is sent as strips")
	fs.StringVar(&opts.tlsCert, "tls-cert", "",
		"PEM certificate to serve HTTPS with. A remote password travels in a "+
			"request body, so without this the gateway must sit behind a proxy that "+
			"terminates TLS")
	fs.StringVar(&opts.tlsKey, "tls-key", "",
		"PEM private key for -tls-cert")
	fs.BoolVar(&opts.secureCookie, "secure-cookie", true,
		"mark the session cookie Secure. Must stay true behind TLS; set it false "+
			"only for plain HTTP on the loopback interface")
	fs.BoolVar(&opts.trustProxy, "trust-proxy", false,
		"believe X-Forwarded-For for login rate limiting. Only set this when a "+
			"reverse proxy is the only way in, or any client can claim any address")
	fs.BoolVar(&opts.demo, "demo", false,
		"serve a synthetic desktop instead of connecting to anything. It draws the "+
			"input it receives, which is useful for checking the UI and the input "+
			"mapping. No remote host is contacted")
	fs.BoolVar(&opts.traceInput, "trace-input", false,
		"raise grdp's log level to TRACE so the bytes of every input PDU are "+
			"logged, for finding out whether a click was sent or ignored. Very "+
			"verbose, and it re-enables the credential logging described "+
			"in the startup warning")
	fs.BoolVar(&opts.debug, "debug", false,
		"raise grdp's log level to INFO. This re-enables the credential logging "+
			"described in the startup warning")
	fs.BoolVar(&opts.showVersion, "version", false, "print the version and exit")

	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() > 0 {
		return options{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	// Remember whether the password came from the command line, so it can be
	// warned about rather than silently accepted.
	opts.fromFlag = false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "password" {
			opts.fromFlag = true
		}
	})
	return opts, nil
}

func resolvePassword(opts options) (string, error) {
	if opts.passwordFile != "" {
		return auth.LoadPassword(opts.passwordFile)
	}
	if opts.password != "" {
		return opts.password, nil
	}
	if env := os.Getenv("ADX_PASSWORD"); env != "" {
		return env, nil
	}
	return "", errors.New("no console password: set -password-file, -password or ADX_PASSWORD")
}

func resolveSecret(opts options) ([]byte, error) {
	if opts.sessionSecret != "" {
		// Hash whatever was given, so a short or oddly shaped value still
		// yields a full-length key rather than a weak one.
		sum := sha256.Sum256([]byte(opts.sessionSecret))
		return sum[:], nil
	}
	return auth.NewSecret(32)
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
