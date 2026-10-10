package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/matthewmgamble/imsgd/internal/config"
)

const agentLabel = "com.mgamble.imsgd"

func agentPlistPath() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, "Library", "LaunchAgents", agentLabel+".plist")
}

func logDir() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, "Library", "Logs", "imsgd")
}

func launchDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

var plistTmpl = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.Exe}}</string>
		<string>run</string>{{if .Config}}
		<string>-config</string>
		<string>{{.Config}}</string>{{end}}
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>IMSGD_LOG_FILE</key><string>{{.LogFile}}</string>
	</dict>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>ThrottleInterval</key><integer>10</integer>
	<key>ProcessType</key><string>Interactive</string>
	<!-- Only crashes and early startup errors land here; normal logs rotate in imsgd.log. -->
	<key>StandardOutPath</key><string>{{.Stdout}}</string>
	<key>StandardErrorPath</key><string>{{.Stdout}}</string>
</dict>
</plist>
`))

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func runInstallAgent(args []string) error {
	fs := flag.NewFlagSet("install-agent", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file to use (default "+config.DefaultPath()+")")
	fs.Parse(args)

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	if strings.Contains(exe, "/go-build") || strings.HasPrefix(exe, os.TempDir()) {
		return fmt.Errorf("%s looks like a temporary build; install the binary first (make install) and run install-agent from there", exe)
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.ValidateForServe(); err != nil {
		return fmt.Errorf("fix the config first (create a key with `imsgd keys create <name>`): %w", err)
	}
	if *cfgPath != "" {
		if *cfgPath, err = filepath.Abs(*cfgPath); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(logDir(), 0o700); err != nil {
		return err
	}
	var buf strings.Builder
	if err := plistTmpl.Execute(&buf, map[string]string{
		"Label": agentLabel, "Exe": xmlEscape(exe), "Config": xmlEscape(*cfgPath),
		"LogFile": xmlEscape(filepath.Join(logDir(), "imsgd.log")),
		"Stdout":  xmlEscape(filepath.Join(logDir(), "launchd.log")),
	}); err != nil {
		return err
	}
	plist := agentPlistPath()
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(plist, []byte(buf.String()), 0o644); err != nil {
		return err
	}
	exec.Command("/bin/launchctl", "bootout", launchDomain()+"/"+agentLabel).Run() // replace if loaded
	if out, err := exec.Command("/bin/launchctl", "bootstrap", launchDomain(), plist).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Printf("Installed %s\n  binary: %s\n  logs:   %s\n\n", plist, exe, filepath.Join(logDir(), "imsgd.log"))
	fmt.Printf("imsgd now starts at login and restarts if it exits.\n")
	fmt.Printf("If this is the first install, grant Full Disk Access to %s\n", exe)
	fmt.Printf("(System Settings → Privacy & Security → Full Disk Access), then run `imsgd status`.\n")
	return nil
}

func runUninstallAgent(args []string) error {
	out, err := exec.Command("/bin/launchctl", "bootout", launchDomain()+"/"+agentLabel).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "No such process") && !strings.Contains(string(out), "Could not find") {
		fmt.Fprintf(os.Stderr, "launchctl bootout: %s\n", strings.TrimSpace(string(out)))
	}
	if err := os.Remove(agentPlistPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Println("Agent removed. Configuration, data and logs were left in place.")
	return nil
}

// agentState returns launchd's view of the agent.
func agentState() (loaded bool, state, pid string) {
	out, err := exec.Command("/bin/launchctl", "print", launchDomain()+"/"+agentLabel).Output()
	if err != nil {
		return false, "", ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "state = "); ok && state == "" {
			state = v
		}
		if v, ok := strings.CutPrefix(line, "pid = "); ok {
			pid = v
		}
	}
	return true, state, pid
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file")
	fs.Parse(args)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	loaded, state, pid := agentState()
	switch {
	case !loaded:
		fmt.Println("agent:   not installed (run `imsgd install-agent`)")
	default:
		fmt.Printf("agent:   %s (pid %s)\n", state, pid)
	}
	scheme := "http"
	client := &http.Client{Timeout: 3 * time.Second}
	if cfg.TLS.CertFile != "" {
		scheme = "https"
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // local health probe only
	}
	host := cfg.Listen
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := fmt.Sprintf("%s://%s/health", scheme, net.JoinHostPort(host, strconv.Itoa(cfg.Port)))
	res, err := client.Get(url)
	if err != nil {
		fmt.Printf("server:  not responding at %s (%v)\n", url, err)
		fmt.Printf("logs:    %s\n", filepath.Join(logDir(), "imsgd.log"))
		return errors.New("not running")
	}
	defer res.Body.Close()
	var body [512]byte
	n, _ := res.Body.Read(body[:])
	fmt.Printf("server:  %s → %d %s\n", url, res.StatusCode, strings.TrimSpace(string(body[:n])))
	for _, ip := range lanAddrs() {
		fmt.Printf("lan:     %s://%s\n", scheme, net.JoinHostPort(ip, strconv.Itoa(cfg.Port)))
	}
	return nil
}

func runKeys(args []string) error {
	usage := errors.New("usage: imsgd keys list | create NAME | revoke NAME  [-config FILE]")
	if len(args) == 0 {
		return usage
	}
	fs := flag.NewFlagSet("keys", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath(), "config file")
	fs.Parse(args[1:])
	switch args[0] {
	case "list":
		names, err := config.KeyNames(*cfgPath)
		if err != nil {
			return err
		}
		if len(names) == 0 {
			fmt.Println("no API keys configured")
		}
		for _, n := range names {
			fmt.Println(n)
		}
	case "create":
		if fs.NArg() != 1 {
			return usage
		}
		key, err := config.AddKey(*cfgPath, fs.Arg(0))
		if err != nil {
			return err
		}
		fmt.Printf("Created API key %q in %s:\n\n  %s\n\nStore it now; use it as `Authorization: Bearer <key>`.\n", fs.Arg(0), *cfgPath, key)
		fmt.Println("A running server picks it up within a few seconds.")
	case "revoke":
		if fs.NArg() != 1 {
			return usage
		}
		if err := config.RemoveKey(*cfgPath, fs.Arg(0)); err != nil {
			return err
		}
		fmt.Printf("Revoked %q. A running server stops accepting it within a few seconds.\n", fs.Arg(0))
	default:
		return usage
	}
	return nil
}

// runGenCert writes a self-signed certificate for TLS on the LAN.
func runGenCert(args []string) error {
	fs := flag.NewFlagSet("gen-cert", flag.ExitOnError)
	dir := fs.String("dir", filepath.Dir(config.DefaultPath()), "output directory")
	days := fs.Int("days", 825, "validity in days")
	extra := fs.String("hosts", "", "extra comma-separated DNS names or IPs")
	force := fs.Bool("force", false, "overwrite existing files")
	fs.Parse(args)

	certPath, keyPath := filepath.Join(*dir, "cert.pem"), filepath.Join(*dir, "key.pem")
	if !*force {
		for _, p := range []string{certPath, keyPath} {
			if _, err := os.Stat(p); err == nil {
				return fmt.Errorf("%s exists (use -force to replace it)", p)
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	hostname, _ := os.Hostname()
	hostname = strings.TrimSuffix(hostname, ".local")
	dns := []string{"localhost"}
	if hostname != "" {
		dns = append(dns, hostname, hostname+".local")
	}
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	for _, a := range lanAddrs() {
		ips = append(ips, net.ParseIP(a))
	}
	for _, h := range strings.Split(*extra, ",") {
		if h = strings.TrimSpace(h); h == "" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			ips = append(ips, ip)
		} else {
			dns = append(dns, h)
		}
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "imsgd " + hostname},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(0, 0, *days),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dns,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	sum := sha256.Sum256(der)
	fmt.Printf("Wrote %s and %s (valid %d days)\n", certPath, keyPath, *days)
	fmt.Printf("Names: %s\nIPs:   %v\n", strings.Join(dns, ", "), ips)
	fmt.Printf("SHA-256 fingerprint (pin this in clients):\n  %X\n\n", sum)
	fmt.Printf("Enable it in %s:\n\ntls:\n  cert_file: %s\n  key_file: %s\n\nthen restart imsgd.\n", config.DefaultPath(), certPath, keyPath)
	return nil
}

// certInfo summarizes a configured certificate for doctor.
func certInfo(certFile, keyFile string) (string, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return "", err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return "", err
	}
	left := time.Until(leaf.NotAfter)
	if left < 0 {
		return "", fmt.Errorf("expired on %s", leaf.NotAfter.Format(time.DateOnly))
	}
	return fmt.Sprintf("valid until %s (%d days); names %v", leaf.NotAfter.Format(time.DateOnly), int(left.Hours()/24), leaf.DNSNames), nil
}

var _ = context.Background
