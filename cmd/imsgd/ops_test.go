package main

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenCertServesTLS(t *testing.T) {
	dir := t.TempDir()
	stdout := os.Stdout
	os.Stdout, _ = os.Open(os.DevNull)
	err := runGenCert([]string{"-dir", dir, "-hosts", "imsgd.example.lan"})
	os.Stdout = stdout
	if err != nil {
		t.Fatal(err)
	}
	if err := runGenCert([]string{"-dir", dir}); err == nil {
		t.Fatal("overwrote existing cert without -force")
	}
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if fi, _ := os.Stat(keyFile); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", fi.Mode().Perm())
	}
	info, err := certInfo(certFile, keyFile)
	if err != nil || !strings.Contains(info, "imsgd.example.lan") {
		t.Fatalf("certInfo = %q, %v", info, err)
	}

	pair, _ := tls.LoadX509KeyPair(certFile, keyFile)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))

	pool := x509.NewCertPool()
	pemBytes, _ := os.ReadFile(certFile)
	pool.AppendCertsFromPEM(pemBytes)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	res, err := client.Get("https://127.0.0.1:" + port + "/") // verifies the IP SAN
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(b) != "ok" {
		t.Fatalf("body %q", b)
	}
}

func TestPlistEscapes(t *testing.T) {
	if got := xmlEscape(`/Users/a&b/<x>"`); got != "/Users/a&amp;b/&lt;x&gt;&quot;" {
		t.Fatal(got)
	}
}
