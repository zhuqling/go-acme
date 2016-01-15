package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"log"
	"net/http"
	"os"
	"io"
	"strings"
)

const (
	ACMEChallengePathPrefix = "/.well-known/acme-challenge/"
	LetsEncryptStaging      = "https://acme-staging.api.letsencrypt.org/directory"
	LetsEncryptProduction   = "https://acme-v01.api.letsencrypt.org/directory"
)

var cfg struct {
	KeyPath string
	Addr    string
	Domains string
	API     string
	Bits    int

	outKeyFile    string
	outCertFile    string
}

func init() {
	log.SetFlags(0) // do not log date
	flag.StringVar(&cfg.KeyPath, "key", "", "path to account key")
	flag.StringVar(&cfg.Addr, "addr", "127.0.0.1:81", "challenge server address")
	flag.StringVar(&cfg.Domains, "domains", "", "comma-separated list of up to 100 domain names")
	flag.StringVar(&cfg.API, "api", LetsEncryptProduction, "ACME API URL")
	flag.IntVar(&cfg.Bits, "bit", 2048, "domain key length")
	flag.StringVar(&cfg.outKeyFile, "keyFile", "privateKey.pem", "path to save private key")
	flag.StringVar(&cfg.outCertFile, "certFile", "chain.pem", "path to save public cert")
	flag.Parse()
}

func main() {
	var err error

	domains := strings.Split(cfg.Domains, ",")
	if len(domains) > 100 {
		log.Fatalf("Too many domains (%d > 100)", len(domains))
	}

	// read the account key from stdin if not given in flags
	keyReader := os.Stdin

	if cfg.KeyPath != "" {
		keyReader, err = os.Open(cfg.KeyPath)
		if err != nil {
			log.Fatalf("Failed to read account key: %s", err)
		}
	}

	key, err := loadRSAKey(keyReader)
	if err != nil {
		log.Fatalf("Failed to parse key: %s", err)
	}

	log.Printf("Connecting to ACME server at %s", cfg.API)
	acme, err := OpenACME(cfg.API, key)
	if err != nil {
		log.Fatalf("Failed to connect to ACME server: %s", err)
	}

	// start the challenge server in background
	log.Printf("Responding to ACME challenges at http://%s", cfg.Addr)
	go http.ListenAndServe(cfg.Addr, acme)

	log.Printf("Registering account key")
	if err := acme.NewReg(); err != nil {
		log.Fatalf("Failed to register account key: %s", err)
	}

	// authorize domains in parallel
	type Done struct {
		Domain string
		Error  error
	}
	ch := make(chan Done)
	for _, domain := range domains {
		go func(domain string) {
			log.Printf("Authorizing domain %s", domain)
			done := Done{Domain: domain}
			if err := acme.NewAuthz(domain); err != nil {
				done.Error = err
			}
			ch <- done
		}(domain)
	}

	// collect authorization result
	failed := false
	for range domains {
		if done := <-ch; done.Error != nil {
			failed = true
			log.Printf("Failed to authorize domain %s: %s", done.Domain, done.Error)
		} else {
			log.Printf("Authorized domain %s", done.Domain)
		}
	}
	if failed {
		log.Fatalln("Some domains failed authorization")
	}

	log.Printf("Generating domain key")
	domainKey, err := rsa.GenerateKey(rand.Reader, cfg.Bits)
	if err != nil {
		log.Fatalf("Failed to generate domain key: %s", err)
	}

	// create certificate signing request
	tpl := &x509.CertificateRequest{DNSNames: domains}
	csr, err := x509.CreateCertificateRequest(rand.Reader, tpl, domainKey)
	if err != nil {
		log.Fatalf("Failed to create certificate request: %s", err)
	}

	log.Printf("Fetching certificates")
	domainCrt, issuerCrt, err := acme.NewCert(csr)
	if err != nil {
		log.Fatalf("Failed to fetch certificates: %s", err)
	}

	var keyOutput = os.Stdout
	if cfg.outKeyFile != "" {
		keyFileWriter, err := os.OpenFile(cfg.outKeyFile, os.O_WRONLY | os.O_CREATE, 0644)
		if err != nil {
			log.Fatalf("Failed to read output key: %s", err)
		}
		keyOutput = keyFileWriter
	}
	defer keyOutput.Close()

	// output domain key and certificates in PEM format
	if err := pem.Encode(keyOutput, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(domainKey),
	}); err != nil {
		log.Fatalln(err)
	}

	var certOutput = os.Stdout
	if cfg.outCertFile != "" {
		certFileWriter, err := os.OpenFile(cfg.outCertFile, os.O_WRONLY | os.O_CREATE, 0644)
		if err != nil {
			log.Fatalf("Failed to read output cert: %s", err)
		}
		certOutput = certFileWriter
	}
	defer certOutput.Close()

	if err := pem.Encode(certOutput, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: domainCrt.Raw,
	}); err != nil {
		log.Fatalln(err)
	}

	if err := pem.Encode(certOutput, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: issuerCrt.Raw,
	}); err != nil {
		log.Fatalln(err)
	}
}
