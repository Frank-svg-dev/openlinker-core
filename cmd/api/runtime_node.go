package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenLinker-ai/openlinker-core/pkg/db"
	"github.com/OpenLinker-ai/openlinker-core/pkg/runtime"
)

const (
	defaultRuntimeNodeVersion  = "openlinker-agent-node/reliable-run-v2"
	defaultRuntimeNodeValidity = 365 * 24 * time.Hour
	minRuntimeNodeValidity     = time.Hour
	maxRuntimeNodeValidity     = 397 * 24 * time.Hour
	runtimeNodeClockSkew       = 5 * time.Minute
	runtimeNodeCommandTimeout  = 30 * time.Second
	runtimeNodeURIPrefix       = "urn:openlinker:runtime-node:"
	runtimeNodeIssueUsage      = "usage: api runtime-node issue --ca-cert FILE --ca-key FILE --display-name NAME --cert-out FILE --key-out FILE [--node-id UUID] [--capacity N] [--valid-for DURATION]"
	runtimeNodeInspectUsage    = "usage: api runtime-node inspect --cert FILE [--key FILE] [--ca-cert FILE]"
)

type runtimeNodeIssueConfig struct {
	DatabaseURL string
	CACertFile  string
	CAKeyFile   string
	CertOut     string
	KeyOut      string
	NodeID      uuid.UUID
	DisplayName string
	NodeVersion string
	Capacity    int32
	ValidFor    time.Duration
}

type runtimeNodeInspectConfig struct {
	CertFile   string
	KeyFile    string
	CACertFile string
}

type runtimeNodeRecord struct {
	NodeID                    uuid.UUID
	DisplayName               string
	DeviceCertificateSerial   string
	DevicePublicKeyThumbprint string
	NodeVersion               string
	Capacity                  int32
	Features                  []string
}

type runtimeNodeCertificate struct {
	CertificatePEM []byte
	PrivateKeyPEM  []byte
	Certificate    *x509.Certificate
	PrivateKey     *ecdsa.PrivateKey
	Record         runtimeNodeRecord
}

type runtimeNodeAudit struct {
	NodeID                    string    `json:"node_id"`
	CertificateSerial         string    `json:"certificate_serial"`
	CertificateSHA256         string    `json:"certificate_sha256"`
	PublicKeyThumbprintSHA256 string    `json:"public_key_thumbprint_sha256"`
	Subject                   string    `json:"subject"`
	Issuer                    string    `json:"issuer"`
	NotBefore                 time.Time `json:"not_before"`
	NotAfter                  time.Time `json:"not_after"`
	CurrentlyValid            bool      `json:"currently_valid"`
	PublicKey                 string    `json:"public_key"`
	KeyUsage                  []string  `json:"key_usage"`
	ExtendedKeyUsage          []string  `json:"extended_key_usage"`
	PrivateKeyMatches         *bool     `json:"private_key_matches,omitempty"`
	CAValidationPerformed     bool      `json:"ca_validation_performed"`
	RuntimeProtocolVersion    int       `json:"runtime_protocol_version"`
	RuntimeContractID         string    `json:"runtime_contract_id"`
	RuntimeContractDigest     string    `json:"runtime_contract_digest"`
	RequiredFeatures          []string  `json:"required_features"`
}

type runtimeNodeIssueOutput struct {
	runtimeNodeAudit
	DisplayName string `json:"display_name"`
	NodeVersion string `json:"node_version"`
	Capacity    int32  `json:"capacity"`
	CertFile    string `json:"certificate_file"`
	KeyFile     string `json:"private_key_file"`
	Registered  bool   `json:"registered"`
}

func runRuntimeNode(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: api runtime-node <issue|inspect> [flags]")
		return 2
	}

	switch args[0] {
	case "issue":
		cfg, err := parseRuntimeNodeIssueConfig(args[1:], getenv)
		if err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprintln(stdout, runtimeNodeIssueUsage)
				return 0
			}
			fmt.Fprintf(stderr, "runtime-node issue: %v\n", err)
			return 2
		}
		ctx, cancel := context.WithTimeout(context.Background(), runtimeNodeCommandTimeout)
		defer cancel()
		output, err := issueRuntimeNode(ctx, cfg, time.Now, cryptorand.Reader)
		if err != nil {
			fmt.Fprintf(stderr, "runtime-node issue: %v\n", err)
			return 1
		}
		if err := writeRuntimeNodeJSON(stdout, output); err != nil {
			fmt.Fprintf(stderr, "runtime-node issue: write output: %v\n", err)
			return 1
		}
		return 0
	case "inspect":
		cfg, err := parseRuntimeNodeInspectConfig(args[1:])
		if err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprintln(stdout, runtimeNodeInspectUsage)
				return 0
			}
			fmt.Fprintf(stderr, "runtime-node inspect: %v\n", err)
			return 2
		}
		audit, err := inspectRuntimeNodeFiles(cfg, time.Now().UTC())
		if err != nil {
			fmt.Fprintf(stderr, "runtime-node inspect: %v\n", err)
			return 1
		}
		if err := writeRuntimeNodeJSON(stdout, audit); err != nil {
			fmt.Fprintf(stderr, "runtime-node inspect: write output: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "runtime-node: unknown command %q; expected issue or inspect\n", args[0])
		return 2
	}
}

func parseRuntimeNodeIssueConfig(args []string, getenv func(string) string) (runtimeNodeIssueConfig, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	cfg := runtimeNodeIssueConfig{
		DatabaseURL: strings.TrimSpace(getenv("DATABASE_URL")),
		NodeVersion: defaultRuntimeNodeVersion,
		Capacity:    1,
		ValidFor:    defaultRuntimeNodeValidity,
	}
	var nodeID string
	capacity := int(cfg.Capacity)
	fs := flag.NewFlagSet("runtime-node issue", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.DatabaseURL, "database-url", cfg.DatabaseURL, "Postgres URL; defaults to DATABASE_URL")
	fs.StringVar(&cfg.CACertFile, "ca-cert", "", "offline client CA certificate PEM")
	fs.StringVar(&cfg.CAKeyFile, "ca-key", "", "offline client CA private key PEM")
	fs.StringVar(&cfg.CertOut, "cert-out", "", "new Node certificate PEM path")
	fs.StringVar(&cfg.KeyOut, "key-out", "", "new Node private key PEM path")
	fs.StringVar(&nodeID, "node-id", "", "Node UUID; generated when omitted")
	fs.StringVar(&cfg.DisplayName, "display-name", "", "operator-facing Node name")
	fs.StringVar(&cfg.NodeVersion, "node-version", cfg.NodeVersion, "exact Agent Node runtime version")
	fs.IntVar(&capacity, "capacity", capacity, "maximum concurrent assignments (1-1024)")
	fs.DurationVar(&cfg.ValidFor, "valid-for", cfg.ValidFor, "certificate lifetime (1h-9528h)")
	if err := fs.Parse(args); err != nil {
		return runtimeNodeIssueConfig{}, err
	}
	if fs.NArg() != 0 {
		return runtimeNodeIssueConfig{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}

	cfg.DatabaseURL = strings.TrimSpace(cfg.DatabaseURL)
	cfg.CACertFile = strings.TrimSpace(cfg.CACertFile)
	cfg.CAKeyFile = strings.TrimSpace(cfg.CAKeyFile)
	cfg.CertOut = strings.TrimSpace(cfg.CertOut)
	cfg.KeyOut = strings.TrimSpace(cfg.KeyOut)
	cfg.DisplayName = strings.TrimSpace(cfg.DisplayName)
	cfg.NodeVersion = strings.TrimSpace(cfg.NodeVersion)
	if capacity < 1 || capacity > 1024 {
		return runtimeNodeIssueConfig{}, errors.New("--capacity must be between 1 and 1024")
	}
	cfg.Capacity = int32(capacity)
	if cfg.DatabaseURL == "" {
		return runtimeNodeIssueConfig{}, errors.New("DATABASE_URL or --database-url is required")
	}
	for _, required := range []struct {
		name  string
		value string
	}{
		{name: "--ca-cert", value: cfg.CACertFile},
		{name: "--ca-key", value: cfg.CAKeyFile},
		{name: "--cert-out", value: cfg.CertOut},
		{name: "--key-out", value: cfg.KeyOut},
		{name: "--display-name", value: cfg.DisplayName},
	} {
		if required.value == "" {
			return runtimeNodeIssueConfig{}, fmt.Errorf("%s is required", required.name)
		}
	}
	if cfg.CertOut == "-" || cfg.KeyOut == "-" {
		return runtimeNodeIssueConfig{}, errors.New("certificate and private key must be written to files")
	}
	if !utf8.ValidString(cfg.DisplayName) || utf8.RuneCountInString(cfg.DisplayName) > 200 {
		return runtimeNodeIssueConfig{}, errors.New("--display-name must contain 1-200 valid UTF-8 characters")
	}
	if !utf8.ValidString(cfg.NodeVersion) || utf8.RuneCountInString(cfg.NodeVersion) < 1 || utf8.RuneCountInString(cfg.NodeVersion) > 100 {
		return runtimeNodeIssueConfig{}, errors.New("--node-version must contain 1-100 valid UTF-8 characters")
	}
	if cfg.ValidFor < minRuntimeNodeValidity || cfg.ValidFor > maxRuntimeNodeValidity {
		return runtimeNodeIssueConfig{}, fmt.Errorf("--valid-for must be between %s and %s", minRuntimeNodeValidity, maxRuntimeNodeValidity)
	}
	if strings.TrimSpace(nodeID) != "" {
		parsed, err := uuid.Parse(strings.TrimSpace(nodeID))
		if err != nil || parsed == uuid.Nil {
			return runtimeNodeIssueConfig{}, errors.New("--node-id must be a non-zero UUID")
		}
		cfg.NodeID = parsed
	}
	if err := validateRuntimeNodeOutputPaths(cfg.CertOut, cfg.KeyOut); err != nil {
		return runtimeNodeIssueConfig{}, err
	}
	return cfg, nil
}

func parseRuntimeNodeInspectConfig(args []string) (runtimeNodeInspectConfig, error) {
	var cfg runtimeNodeInspectConfig
	fs := flag.NewFlagSet("runtime-node inspect", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.CertFile, "cert", "", "Node certificate PEM")
	fs.StringVar(&cfg.KeyFile, "key", "", "optional Node private key PEM")
	fs.StringVar(&cfg.CACertFile, "ca-cert", "", "optional client CA certificate PEM")
	if err := fs.Parse(args); err != nil {
		return runtimeNodeInspectConfig{}, err
	}
	if fs.NArg() != 0 {
		return runtimeNodeInspectConfig{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}
	cfg.CertFile = strings.TrimSpace(cfg.CertFile)
	cfg.KeyFile = strings.TrimSpace(cfg.KeyFile)
	cfg.CACertFile = strings.TrimSpace(cfg.CACertFile)
	if cfg.CertFile == "" {
		return runtimeNodeInspectConfig{}, errors.New("--cert is required")
	}
	return cfg, nil
}

func issueRuntimeNode(ctx context.Context, cfg runtimeNodeIssueConfig, now func() time.Time, random io.Reader) (output runtimeNodeIssueOutput, returnedErr error) {
	if now == nil {
		now = time.Now
	}
	if random == nil {
		random = cryptorand.Reader
	}
	if cfg.NodeID == uuid.Nil {
		generated, err := uuid.NewRandomFromReader(random)
		if err != nil {
			return runtimeNodeIssueOutput{}, fmt.Errorf("generate Node ID: %w", err)
		}
		cfg.NodeID = generated
	}
	if err := ensureRuntimeNodeOutputsAbsent(cfg.CertOut, cfg.KeyOut); err != nil {
		return runtimeNodeIssueOutput{}, err
	}

	caCertificate, caSigner, err := loadRuntimeNodeCA(cfg.CACertFile, cfg.CAKeyFile, now().UTC())
	if err != nil {
		return runtimeNodeIssueOutput{}, err
	}
	bundle, err := createRuntimeNodeCertificate(cfg, caCertificate, caSigner, now().UTC(), random)
	if err != nil {
		return runtimeNodeIssueOutput{}, err
	}
	audit, err := auditRuntimeNodeCertificate(bundle.Certificate, bundle.PrivateKey, caCertificate, now().UTC())
	if err != nil {
		return runtimeNodeIssueOutput{}, fmt.Errorf("audit issued certificate: %w", err)
	}
	artifacts, err := stageRuntimeNodeArtifacts(cfg.CertOut, bundle.CertificatePEM, cfg.KeyOut, bundle.PrivateKeyPEM)
	if err != nil {
		return runtimeNodeIssueOutput{}, err
	}
	defer func() {
		if err := artifacts.cleanupStaged(); err != nil {
			returnedErr = errors.Join(returnedErr, fmt.Errorf("clean staged certificate files: %w", err))
		}
	}()

	pool, err := db.Connect(ctx, cfg.DatabaseURL, db.PoolOptions{MaxConns: 1, MinConns: 0})
	if err != nil {
		return runtimeNodeIssueOutput{}, fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()

	published := false
	err = registerRuntimeNode(ctx, pool, bundle.Record, func() error {
		if err := artifacts.publish(); err != nil {
			return err
		}
		published = true
		return nil
	})
	if err != nil {
		var commitErr *runtimeNodeCommitError
		if published && errors.As(err, &commitErr) {
			registered, checkErr := runtimeNodeRegistrationExists(ctx, pool, bundle.Record)
			switch {
			case checkErr == nil && registered:
				err = nil
			case checkErr != nil:
				return runtimeNodeIssueOutput{}, fmt.Errorf("database commit outcome is unknown; complete certificate files were retained for audit: %w (verification failed: %v)", err, checkErr)
			default:
				if cleanupErr := artifacts.cleanupPublished(); cleanupErr != nil {
					return runtimeNodeIssueOutput{}, fmt.Errorf("%w; cleanup certificate files: %v", err, cleanupErr)
				}
			}
		} else if published {
			if cleanupErr := artifacts.cleanupPublished(); cleanupErr != nil {
				return runtimeNodeIssueOutput{}, fmt.Errorf("%w; cleanup certificate files: %v", err, cleanupErr)
			}
		}
		if err != nil {
			return runtimeNodeIssueOutput{}, err
		}
	}

	certPath, _ := filepath.Abs(cfg.CertOut)
	keyPath, _ := filepath.Abs(cfg.KeyOut)
	return runtimeNodeIssueOutput{
		runtimeNodeAudit: audit,
		DisplayName:      cfg.DisplayName,
		NodeVersion:      cfg.NodeVersion,
		Capacity:         cfg.Capacity,
		CertFile:         certPath,
		KeyFile:          keyPath,
		Registered:       true,
	}, nil
}

func loadRuntimeNodeCA(certFile, keyFile string, now time.Time) (*x509.Certificate, crypto.Signer, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read client CA certificate: %w", err)
	}
	certificate, err := parseSingleCertificatePEM(certPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("parse client CA certificate: %w", err)
	}
	keyHandle, err := os.Open(keyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("open client CA private key: %w", err)
	}
	keyInfo, statErr := keyHandle.Stat()
	if statErr != nil {
		_ = keyHandle.Close()
		return nil, nil, fmt.Errorf("inspect client CA private key: %w", statErr)
	}
	if !keyInfo.Mode().IsRegular() {
		_ = keyHandle.Close()
		return nil, nil, errors.New("client CA private key must be a regular file")
	}
	if goruntime.GOOS != "windows" && keyInfo.Mode().Perm()&0o077 != 0 {
		_ = keyHandle.Close()
		return nil, nil, fmt.Errorf("client CA private key permissions must be owner-only (got %04o)", keyInfo.Mode().Perm())
	}
	keyPEM, err := io.ReadAll(keyHandle)
	if err != nil {
		_ = keyHandle.Close()
		return nil, nil, fmt.Errorf("read client CA private key: %w", err)
	}
	if err := keyHandle.Close(); err != nil {
		return nil, nil, fmt.Errorf("close client CA private key: %w", err)
	}
	signer, err := parsePrivateKeyPEM(keyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("parse client CA private key: %w", err)
	}
	if !certificate.BasicConstraintsValid || !certificate.IsCA || certificate.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, nil, errors.New("client CA certificate must be a CA with keyCertSign usage")
	}
	if now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) {
		return nil, nil, errors.New("client CA certificate is not currently valid")
	}
	if !caAllowsClientAuth(certificate.ExtKeyUsage) {
		return nil, nil, errors.New("client CA extended key usage does not allow clientAuth")
	}
	if insecureCertificateSignatureAlgorithm(certificate.SignatureAlgorithm) {
		return nil, nil, errors.New("client CA certificate uses an insecure signature algorithm")
	}
	if err := validateRuntimeNodeCAPublicKey(certificate.PublicKey); err != nil {
		return nil, nil, err
	}
	certPublicKey, err := x509.MarshalPKIXPublicKey(certificate.PublicKey)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal client CA public key: %w", err)
	}
	signerPublicKey, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return nil, nil, fmt.Errorf("marshal client CA private-key public part: %w", err)
	}
	if !bytes.Equal(certPublicKey, signerPublicKey) {
		return nil, nil, errors.New("client CA certificate and private key do not match")
	}
	return certificate, signer, nil
}

func createRuntimeNodeCertificate(cfg runtimeNodeIssueConfig, caCertificate *x509.Certificate, caSigner crypto.Signer, now time.Time, random io.Reader) (runtimeNodeCertificate, error) {
	if caCertificate == nil || caSigner == nil {
		return runtimeNodeCertificate{}, errors.New("client CA certificate and signer are required")
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), random)
	if err != nil {
		return runtimeNodeCertificate{}, fmt.Errorf("generate P-256 private key: %w", err)
	}
	serial, err := randomRuntimeNodeSerial(random)
	if err != nil {
		return runtimeNodeCertificate{}, err
	}
	notBefore := now.UTC().Truncate(time.Second).Add(-runtimeNodeClockSkew)
	if notBefore.Before(caCertificate.NotBefore) {
		notBefore = caCertificate.NotBefore.UTC().Truncate(time.Second)
	}
	notAfter := now.UTC().Truncate(time.Second).Add(cfg.ValidFor)
	if notAfter.After(caCertificate.NotAfter) {
		return runtimeNodeCertificate{}, fmt.Errorf("client CA expires before requested Node certificate (%s)", notAfter.Format(time.RFC3339))
	}
	if !notAfter.After(notBefore) {
		return runtimeNodeCertificate{}, errors.New("Node certificate validity window is empty")
	}
	nodeURI, err := url.Parse(runtimeNodeURIPrefix + cfg.NodeID.String())
	if err != nil {
		return runtimeNodeCertificate{}, fmt.Errorf("build Node identity URI: %w", err)
	}
	publicKeyDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		return runtimeNodeCertificate{}, fmt.Errorf("marshal Node public key: %w", err)
	}
	publicKeyDigest := sha256.Sum256(publicKeyDER)
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"OpenLinker Runtime"},
			CommonName:   "runtime-node-" + cfg.NodeID.String(),
			SerialNumber: cfg.NodeID.String(),
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		SubjectKeyId:          append([]byte(nil), publicKeyDigest[:]...),
		AuthorityKeyId:        append([]byte(nil), caCertificate.SubjectKeyId...),
		URIs:                  []*url.URL{nodeURI},
	}
	der, err := x509.CreateCertificate(random, template, caCertificate, &privateKey.PublicKey, caSigner)
	if err != nil {
		return runtimeNodeCertificate{}, fmt.Errorf("sign Node certificate: %w", err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return runtimeNodeCertificate{}, fmt.Errorf("parse issued Node certificate: %w", err)
	}
	privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return runtimeNodeCertificate{}, fmt.Errorf("marshal Node private key: %w", err)
	}
	return runtimeNodeCertificate{
		CertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		PrivateKeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER}),
		Certificate:    certificate,
		PrivateKey:     privateKey,
		Record: runtimeNodeRecord{
			NodeID:                    cfg.NodeID,
			DisplayName:               cfg.DisplayName,
			DeviceCertificateSerial:   strings.ToLower(serial.Text(16)),
			DevicePublicKeyThumbprint: hex.EncodeToString(publicKeyDigest[:]),
			NodeVersion:               cfg.NodeVersion,
			Capacity:                  cfg.Capacity,
			Features:                  runtime.RuntimeRequiredFeatures(),
		},
	}, nil
}

func randomRuntimeNodeSerial(random io.Reader) (*big.Int, error) {
	// One fixed non-zero prefix plus 128 random bits yields a positive 129-bit
	// serial with at least 128 bits of entropy and stays below RFC 5280's
	// 20-octet serial-number limit.
	serialBytes := make([]byte, 17)
	serialBytes[0] = 1
	if _, err := io.ReadFull(random, serialBytes[1:]); err != nil {
		return nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	return new(big.Int).SetBytes(serialBytes), nil
}

func parseSingleCertificatePEM(data []byte) (*x509.Certificate, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("expected exactly one CERTIFICATE PEM block")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("expected exactly one CERTIFICATE PEM block")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	return certificate, nil
}

func parsePrivateKeyPEM(data []byte) (crypto.Signer, error) {
	var keyBlock *pem.Block
	rest := data
	seenECParameters := false
	for len(bytes.TrimSpace(rest)) != 0 {
		block, remaining := pem.Decode(rest)
		if block == nil {
			return nil, errors.New("private-key file contains invalid PEM data")
		}
		rest = remaining
		if block.Type == "EC PARAMETERS" && !seenECParameters && keyBlock == nil {
			// OpenSSL's common `ecparam -genkey` output prefixes an EC PRIVATE
			// KEY with this redundant public parameter block. The private key
			// itself still carries the authoritative curve and is validated below.
			seenECParameters = true
			continue
		}
		if keyBlock != nil {
			return nil, errors.New("expected exactly one unencrypted private key")
		}
		keyBlock = block
	}
	if keyBlock == nil {
		return nil, errors.New("expected exactly one unencrypted private key")
	}
	var key any
	var err error
	switch keyBlock.Type {
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(keyBlock.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	default:
		return nil, fmt.Errorf("unsupported or encrypted private-key PEM type %q", keyBlock.Type)
	}
	if err != nil {
		return nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, errors.New("private key does not implement crypto.Signer")
	}
	return signer, nil
}

func caAllowsClientAuth(usages []x509.ExtKeyUsage) bool {
	if len(usages) == 0 {
		return true
	}
	for _, usage := range usages {
		if usage == x509.ExtKeyUsageAny || usage == x509.ExtKeyUsageClientAuth {
			return true
		}
	}
	return false
}

func insecureCertificateSignatureAlgorithm(algorithm x509.SignatureAlgorithm) bool {
	switch algorithm {
	case x509.MD2WithRSA, x509.MD5WithRSA, x509.SHA1WithRSA, x509.ECDSAWithSHA1:
		return true
	default:
		return false
	}
}

func validateRuntimeNodeCAPublicKey(publicKey any) error {
	switch key := publicKey.(type) {
	case *rsa.PublicKey:
		if key.N == nil || key.N.BitLen() < 2048 {
			return errors.New("client CA RSA key must be at least 2048 bits")
		}
	case *ecdsa.PublicKey:
		if key.Curve == nil || key.Curve.Params().BitSize < 256 {
			return errors.New("client CA ECDSA key must use P-256 or stronger")
		}
	case ed25519.PublicKey:
		if len(key) != ed25519.PublicKeySize {
			return errors.New("client CA Ed25519 public key is invalid")
		}
	default:
		return fmt.Errorf("client CA public key type %T is not supported", publicKey)
	}
	return nil
}

type stagedRuntimeNodeArtifacts struct {
	certOut  string
	keyOut   string
	certTemp string
	keyTemp  string
}

func validateRuntimeNodeOutputPaths(certOut, keyOut string) error {
	certAbs, err := filepath.Abs(certOut)
	if err != nil {
		return fmt.Errorf("resolve --cert-out: %w", err)
	}
	keyAbs, err := filepath.Abs(keyOut)
	if err != nil {
		return fmt.Errorf("resolve --key-out: %w", err)
	}
	certParent, err := filepath.EvalSymlinks(filepath.Dir(certAbs))
	if err != nil {
		return fmt.Errorf("resolve --cert-out parent: %w", err)
	}
	keyParent, err := filepath.EvalSymlinks(filepath.Dir(keyAbs))
	if err != nil {
		return fmt.Errorf("resolve --key-out parent: %w", err)
	}
	if filepath.Join(certParent, filepath.Base(certAbs)) == filepath.Join(keyParent, filepath.Base(keyAbs)) {
		return errors.New("--cert-out and --key-out must be different files")
	}
	return nil
}

func ensureRuntimeNodeOutputsAbsent(paths ...string) error {
	for _, path := range paths {
		_, err := os.Lstat(path)
		switch {
		case err == nil:
			return fmt.Errorf("refusing to overwrite existing file %s", path)
		case errors.Is(err, os.ErrNotExist):
			continue
		default:
			return fmt.Errorf("inspect output path %s: %w", path, err)
		}
	}
	return nil
}

func stageRuntimeNodeArtifacts(certOut string, certPEM []byte, keyOut string, keyPEM []byte) (*stagedRuntimeNodeArtifacts, error) {
	if err := validateRuntimeNodeOutputPaths(certOut, keyOut); err != nil {
		return nil, err
	}
	if err := ensureRuntimeNodeOutputsAbsent(certOut, keyOut); err != nil {
		return nil, err
	}
	staged := &stagedRuntimeNodeArtifacts{certOut: certOut, keyOut: keyOut}
	var err error
	staged.keyTemp, err = writeRuntimeNodeTemp(keyOut, keyPEM, 0o600)
	if err != nil {
		return nil, fmt.Errorf("stage Node private key: %w", err)
	}
	staged.certTemp, err = writeRuntimeNodeTemp(certOut, certPEM, 0o644)
	if err != nil {
		cleanupErr := staged.cleanupStaged()
		return nil, errors.Join(fmt.Errorf("stage Node certificate: %w", err), cleanupErr)
	}
	return staged, nil
}

func writeRuntimeNodeTemp(destination string, data []byte, mode os.FileMode) (path string, returnedErr error) {
	file, err := os.CreateTemp(filepath.Dir(destination), "."+filepath.Base(destination)+".tmp-*")
	if err != nil {
		return "", err
	}
	tempPath := file.Name()
	path = tempPath
	defer func() {
		if returnedErr != nil {
			_ = file.Close()
			removeErr := os.Remove(tempPath)
			syncErr := syncRuntimeNodeDirectories(destination)
			returnedErr = errors.Join(
				returnedErr,
				wrapRuntimeNodeCleanupError("remove staged certificate file", removeErr),
				wrapRuntimeNodeCleanupError("sync certificate directory after staging failure", syncErr),
			)
		}
	}()
	if err := file.Chmod(mode); err != nil {
		return "", err
	}
	if _, err := file.Write(data); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return path, nil
}

func (a *stagedRuntimeNodeArtifacts) publish() error {
	if a == nil {
		return errors.New("staged certificate files are missing")
	}
	if err := os.Link(a.keyTemp, a.keyOut); err != nil {
		return fmt.Errorf("publish Node private key without overwrite: %w", err)
	}
	if err := os.Link(a.certTemp, a.certOut); err != nil {
		removeErr := os.Remove(a.keyOut)
		syncErr := syncRuntimeNodeDirectories(a.keyOut)
		return errors.Join(
			fmt.Errorf("publish Node certificate without overwrite: %w", err),
			wrapRuntimeNodeCleanupError("remove partially published Node private key", removeErr),
			wrapRuntimeNodeCleanupError("sync Node private-key directory", syncErr),
		)
	}
	if err := os.Remove(a.keyTemp); err != nil {
		cleanupErr := a.cleanupPublished()
		return errors.Join(fmt.Errorf("remove staged Node private key: %w", err), cleanupErr)
	}
	a.keyTemp = ""
	if err := os.Remove(a.certTemp); err != nil {
		cleanupErr := a.cleanupPublished()
		return errors.Join(fmt.Errorf("remove staged Node certificate: %w", err), cleanupErr)
	}
	a.certTemp = ""
	if err := syncRuntimeNodeDirectories(a.certOut, a.keyOut); err != nil {
		cleanupErr := a.cleanupPublished()
		return errors.Join(fmt.Errorf("sync published certificate directories: %w", err), cleanupErr)
	}
	return nil
}

func (a *stagedRuntimeNodeArtifacts) cleanupStaged() error {
	if a == nil {
		return nil
	}
	var errs []error
	removed := false
	if a.certTemp != "" {
		if err := os.Remove(a.certTemp); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove staged Node certificate: %w", err))
		} else if err == nil {
			removed = true
		}
	}
	if a.keyTemp != "" {
		if err := os.Remove(a.keyTemp); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove staged Node private key: %w", err))
		} else if err == nil {
			removed = true
		}
	}
	if removed {
		if err := syncRuntimeNodeDirectories(a.certOut, a.keyOut); err != nil {
			errs = append(errs, fmt.Errorf("sync certificate directories after staged cleanup: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (a *stagedRuntimeNodeArtifacts) cleanupPublished() error {
	if a == nil {
		return nil
	}
	var errs []error
	for _, path := range []string{a.certOut, a.keyOut} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
		}
	}
	if err := syncRuntimeNodeDirectories(a.certOut, a.keyOut); err != nil {
		errs = append(errs, fmt.Errorf("sync certificate directories after cleanup: %w", err))
	}
	return errors.Join(errs...)
}

func wrapRuntimeNodeCleanupError(operation string, err error) error {
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func syncRuntimeNodeDirectories(paths ...string) error {
	// Windows does not expose a portable directory fsync through os.File.Sync.
	// The hard-link no-overwrite behavior still applies there; Unix deployments
	// additionally persist directory entries before the database commit returns.
	if goruntime.GOOS == "windows" {
		return nil
	}
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		directory, err := filepath.Abs(filepath.Dir(path))
		if err != nil {
			return fmt.Errorf("resolve output directory for %s: %w", path, err)
		}
		directory, err = filepath.EvalSymlinks(directory)
		if err != nil {
			return fmt.Errorf("resolve output directory %s: %w", directory, err)
		}
		if _, duplicate := seen[directory]; duplicate {
			continue
		}
		seen[directory] = struct{}{}
		dir, err := os.Open(directory)
		if err != nil {
			return fmt.Errorf("open output directory %s: %w", directory, err)
		}
		syncErr := dir.Sync()
		closeErr := dir.Close()
		if syncErr != nil {
			return fmt.Errorf("fsync output directory %s: %w", directory, syncErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close output directory %s: %w", directory, closeErr)
		}
	}
	return nil
}

type runtimeNodeCommitError struct{ err error }

func (e *runtimeNodeCommitError) Error() string {
	return "commit Runtime Node registration: " + e.err.Error()
}
func (e *runtimeNodeCommitError) Unwrap() error { return e.err }

func registerRuntimeNode(ctx context.Context, pool *pgxpool.Pool, record runtimeNodeRecord, beforeCommit func() error) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("begin Runtime Node registration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var contractID, contractDigest string
	if err := tx.QueryRow(ctx, `
SELECT runtime_contract_id, runtime_contract_digest
FROM runtime_schema_contracts
WHERE is_current
FOR SHARE
`).Scan(&contractID, &contractDigest); err != nil {
		return fmt.Errorf("read current runtime contract: %w", err)
	}
	if contractID != runtime.RuntimeContractID || contractDigest != runtime.RuntimeContractDigest {
		return fmt.Errorf("database runtime contract is %s/%s; expected %s/%s", contractID, contractDigest, runtime.RuntimeContractID, runtime.RuntimeContractDigest)
	}

	var insertedNodeID uuid.UUID
	err = tx.QueryRow(ctx, `
INSERT INTO runtime_nodes (
    node_id,
    display_name,
    device_certificate_serial,
    device_public_key_thumbprint,
    node_version,
    protocol_version,
    runtime_contract_id,
    runtime_contract_digest,
    features,
    capacity
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING node_id
`, record.NodeID, record.DisplayName, record.DeviceCertificateSerial,
		record.DevicePublicKeyThumbprint, record.NodeVersion, runtime.RuntimeProtocolVersion,
		runtime.RuntimeContractID, runtime.RuntimeContractDigest, record.Features, record.Capacity,
	).Scan(&insertedNodeID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return fmt.Errorf("Runtime Node identity is already registered (%s)", pgErr.ConstraintName)
		}
		return fmt.Errorf("insert Runtime Node: %w", err)
	}
	if insertedNodeID != record.NodeID {
		return errors.New("database returned a different Runtime Node ID")
	}
	if beforeCommit == nil {
		return errors.New("certificate publisher is required")
	}
	if err := beforeCommit(); err != nil {
		return fmt.Errorf("publish certificate files: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return &runtimeNodeCommitError{err: err}
	}
	return nil
}

func runtimeNodeRegistrationExists(ctx context.Context, pool *pgxpool.Pool, record runtimeNodeRecord) (bool, error) {
	var exists bool
	err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM runtime_nodes
    WHERE node_id = $1
      AND device_certificate_serial = $2
      AND device_public_key_thumbprint = $3
      AND protocol_version = $4
      AND runtime_contract_id = $5
      AND runtime_contract_digest = $6
      AND status <> 'revoked'
)
`, record.NodeID, record.DeviceCertificateSerial, record.DevicePublicKeyThumbprint,
		runtime.RuntimeProtocolVersion, runtime.RuntimeContractID, runtime.RuntimeContractDigest,
	).Scan(&exists)
	return exists, err
}

func inspectRuntimeNodeFiles(cfg runtimeNodeInspectConfig, now time.Time) (runtimeNodeAudit, error) {
	certPEM, err := os.ReadFile(cfg.CertFile)
	if err != nil {
		return runtimeNodeAudit{}, fmt.Errorf("read Node certificate: %w", err)
	}
	certificate, err := parseSingleCertificatePEM(certPEM)
	if err != nil {
		return runtimeNodeAudit{}, fmt.Errorf("parse Node certificate: %w", err)
	}
	var signer crypto.Signer
	if cfg.KeyFile != "" {
		keyPEM, err := os.ReadFile(cfg.KeyFile)
		if err != nil {
			return runtimeNodeAudit{}, fmt.Errorf("read Node private key: %w", err)
		}
		signer, err = parsePrivateKeyPEM(keyPEM)
		if err != nil {
			return runtimeNodeAudit{}, fmt.Errorf("parse Node private key: %w", err)
		}
	}
	var caCertificate *x509.Certificate
	if cfg.CACertFile != "" {
		caPEM, err := os.ReadFile(cfg.CACertFile)
		if err != nil {
			return runtimeNodeAudit{}, fmt.Errorf("read client CA certificate: %w", err)
		}
		caCertificate, err = parseSingleCertificatePEM(caPEM)
		if err != nil {
			return runtimeNodeAudit{}, fmt.Errorf("parse client CA certificate: %w", err)
		}
	}
	return auditRuntimeNodeCertificate(certificate, signer, caCertificate, now)
}

func auditRuntimeNodeCertificate(certificate *x509.Certificate, signer crypto.Signer, caCertificate *x509.Certificate, now time.Time) (runtimeNodeAudit, error) {
	if certificate == nil || certificate.SerialNumber == nil || certificate.SerialNumber.Sign() <= 0 {
		return runtimeNodeAudit{}, errors.New("Node certificate has no positive serial number")
	}
	publicKey, ok := certificate.PublicKey.(*ecdsa.PublicKey)
	if !ok || publicKey.Curve != elliptic.P256() {
		return runtimeNodeAudit{}, errors.New("Node certificate public key must be ECDSA P-256")
	}
	if !certificate.BasicConstraintsValid || certificate.IsCA {
		return runtimeNodeAudit{}, errors.New("Node certificate must be a non-CA certificate")
	}
	if certificate.KeyUsage != x509.KeyUsageDigitalSignature {
		return runtimeNodeAudit{}, errors.New("Node certificate must have digitalSignature key usage only")
	}
	if len(certificate.ExtKeyUsage) != 1 || certificate.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		return runtimeNodeAudit{}, errors.New("Node certificate must have clientAuth extended key usage only")
	}
	nodeID, err := runtimeNodeIDFromCertificate(certificate)
	if err != nil {
		return runtimeNodeAudit{}, err
	}
	if certificate.Subject.SerialNumber != nodeID.String() {
		return runtimeNodeAudit{}, errors.New("Node certificate subject serialNumber does not match its identity URI")
	}
	if caCertificate != nil {
		roots := x509.NewCertPool()
		roots.AddCert(caCertificate)
		if _, err := certificate.Verify(x509.VerifyOptions{
			Roots:       roots,
			CurrentTime: now,
			KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}); err != nil {
			return runtimeNodeAudit{}, fmt.Errorf("verify Node certificate against client CA: %w", err)
		}
	}
	var privateKeyMatches *bool
	if signer != nil {
		certPublicKey, err := x509.MarshalPKIXPublicKey(certificate.PublicKey)
		if err != nil {
			return runtimeNodeAudit{}, fmt.Errorf("marshal Node certificate public key: %w", err)
		}
		keyPublic, err := x509.MarshalPKIXPublicKey(signer.Public())
		if err != nil {
			return runtimeNodeAudit{}, fmt.Errorf("marshal Node private-key public part: %w", err)
		}
		matches := bytes.Equal(certPublicKey, keyPublic)
		privateKeyMatches = &matches
		if !matches {
			return runtimeNodeAudit{}, errors.New("Node certificate and private key do not match")
		}
	}
	fingerprint := sha256.Sum256(certificate.Raw)
	thumbprint := sha256.Sum256(certificate.RawSubjectPublicKeyInfo)
	return runtimeNodeAudit{
		NodeID:                    nodeID.String(),
		CertificateSerial:         strings.ToLower(certificate.SerialNumber.Text(16)),
		CertificateSHA256:         hex.EncodeToString(fingerprint[:]),
		PublicKeyThumbprintSHA256: hex.EncodeToString(thumbprint[:]),
		Subject:                   certificate.Subject.String(),
		Issuer:                    certificate.Issuer.String(),
		NotBefore:                 certificate.NotBefore.UTC(),
		NotAfter:                  certificate.NotAfter.UTC(),
		CurrentlyValid:            !now.Before(certificate.NotBefore) && now.Before(certificate.NotAfter),
		PublicKey:                 "ECDSA P-256",
		KeyUsage:                  []string{"digital_signature"},
		ExtendedKeyUsage:          []string{"client_auth"},
		PrivateKeyMatches:         privateKeyMatches,
		CAValidationPerformed:     caCertificate != nil,
		RuntimeProtocolVersion:    runtime.RuntimeProtocolVersion,
		RuntimeContractID:         runtime.RuntimeContractID,
		RuntimeContractDigest:     runtime.RuntimeContractDigest,
		RequiredFeatures:          runtime.RuntimeRequiredFeatures(),
	}, nil
}

func runtimeNodeIDFromCertificate(certificate *x509.Certificate) (uuid.UUID, error) {
	var nodeID uuid.UUID
	for _, identityURI := range certificate.URIs {
		value := identityURI.String()
		if !strings.HasPrefix(value, runtimeNodeURIPrefix) {
			continue
		}
		if nodeID != uuid.Nil {
			return uuid.Nil, errors.New("Node certificate contains multiple runtime identity URIs")
		}
		parsed, err := uuid.Parse(strings.TrimPrefix(value, runtimeNodeURIPrefix))
		if err != nil || parsed == uuid.Nil {
			return uuid.Nil, errors.New("Node certificate contains an invalid runtime identity URI")
		}
		nodeID = parsed
	}
	if nodeID == uuid.Nil {
		return uuid.Nil, errors.New("Node certificate is missing its runtime identity URI")
	}
	return nodeID, nil
}

func writeRuntimeNodeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
