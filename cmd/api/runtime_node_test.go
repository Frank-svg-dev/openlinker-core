package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/OpenLinker-ai/openlinker-core/pkg/runtime"
)

type runtimeNodeTestCA struct {
	certificate *x509.Certificate
	privateKey  *ecdsa.PrivateKey
	certPEM     []byte
	keyPEM      []byte
}

func TestParseRuntimeNodeIssueConfigStrictAndDefaults(t *testing.T) {
	dir := t.TempDir()
	nodeID := uuid.New()
	args := []string{
		"--ca-cert", filepath.Join(dir, "client-ca.crt"),
		"--ca-key", filepath.Join(dir, "client-ca.key"),
		"--cert-out", filepath.Join(dir, "node.crt"),
		"--key-out", filepath.Join(dir, "node.key"),
		"--node-id", nodeID.String(),
		"--display-name", "Edge worker",
	}
	cfg, err := parseRuntimeNodeIssueConfig(args, func(key string) string {
		if key == "DATABASE_URL" {
			return " postgres://runtime.test/openlinker "
		}
		return ""
	})
	if err != nil {
		t.Fatalf("parseRuntimeNodeIssueConfig() error = %v", err)
	}
	if cfg.DatabaseURL != "postgres://runtime.test/openlinker" || cfg.NodeID != nodeID || cfg.Capacity != 1 ||
		cfg.NodeVersion != defaultRuntimeNodeVersion || cfg.ValidFor != defaultRuntimeNodeValidity {
		t.Fatalf("unexpected config: %#v", cfg)
	}

	tests := []struct {
		name    string
		mutate  func([]string) []string
		wantErr string
	}{
		{
			name: "zero capacity",
			mutate: func(in []string) []string {
				return append(in, "--capacity", "0")
			},
			wantErr: "--capacity must be between 1 and 1024",
		},
		{
			name: "short validity",
			mutate: func(in []string) []string {
				return append(in, "--valid-for", "59m")
			},
			wantErr: "--valid-for must be between",
		},
		{
			name: "positional argument",
			mutate: func(in []string) []string {
				return append(in, "unexpected")
			},
			wantErr: "unexpected positional arguments",
		},
		{
			name: "same output",
			mutate: func(in []string) []string {
				out := append([]string(nil), in...)
				for i := range out {
					if out[i] == "--key-out" {
						out[i+1] = filepath.Join(dir, "node.crt")
					}
				}
				return out
			},
			wantErr: "must be different files",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseRuntimeNodeIssueConfig(tt.mutate(append([]string(nil), args...)), func(string) string {
				return "postgres://runtime.test/openlinker"
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseRuntimeNodeIssueConfigRequiresDatabaseAndFiles(t *testing.T) {
	_, err := parseRuntimeNodeIssueConfig(nil, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("missing database error = %v", err)
	}

	dir := t.TempDir()
	_, err = parseRuntimeNodeIssueConfig([]string{
		"--ca-cert", filepath.Join(dir, "ca.crt"),
		"--ca-key", filepath.Join(dir, "ca.key"),
		"--cert-out", filepath.Join(dir, "node.crt"),
		"--key-out", filepath.Join(dir, "node.key"),
	}, func(string) string { return "postgres://runtime.test/openlinker" })
	if err == nil || !strings.Contains(err.Error(), "--display-name is required") {
		t.Fatalf("missing display-name error = %v", err)
	}
}

func TestCreateRuntimeNodeCertificateHasStrictIdentity(t *testing.T) {
	now := time.Date(2026, time.July, 11, 8, 30, 0, 0, time.UTC)
	ca := newRuntimeNodeTestCA(t, now)
	nodeID := uuid.New()
	cfg := runtimeNodeIssueConfig{
		NodeID:      nodeID,
		DisplayName: "Singapore edge",
		NodeVersion: defaultRuntimeNodeVersion,
		Capacity:    8,
		ValidFor:    90 * 24 * time.Hour,
	}
	bundle, err := createRuntimeNodeCertificate(cfg, ca.certificate, ca.privateKey, now, rand.Reader)
	if err != nil {
		t.Fatalf("createRuntimeNodeCertificate() error = %v", err)
	}
	certificate := bundle.Certificate
	if _, ok := certificate.PublicKey.(*ecdsa.PublicKey); !ok {
		t.Fatalf("public key type = %T, want ECDSA", certificate.PublicKey)
	}
	if certificate.PublicKey.(*ecdsa.PublicKey).Curve != elliptic.P256() {
		t.Fatal("Node certificate curve is not P-256")
	}
	if certificate.SerialNumber.Sign() <= 0 || certificate.SerialNumber.BitLen() < 128 {
		t.Fatalf("serial = %s (%d bits), want positive >=128-bit", certificate.SerialNumber, certificate.SerialNumber.BitLen())
	}
	if certificate.IsCA || !certificate.BasicConstraintsValid || certificate.KeyUsage != x509.KeyUsageDigitalSignature ||
		!reflect.DeepEqual(certificate.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}) {
		t.Fatalf("unsafe certificate usages: is_ca=%v key_usage=%v eku=%v", certificate.IsCA, certificate.KeyUsage, certificate.ExtKeyUsage)
	}
	if certificate.Subject.SerialNumber != nodeID.String() {
		t.Fatalf("subject serialNumber = %q, want %s", certificate.Subject.SerialNumber, nodeID)
	}
	parsedNodeID, err := runtimeNodeIDFromCertificate(certificate)
	if err != nil || parsedNodeID != nodeID {
		t.Fatalf("runtimeNodeIDFromCertificate() = %s, %v", parsedNodeID, err)
	}
	if err := certificate.CheckSignatureFrom(ca.certificate); err != nil {
		t.Fatalf("certificate signature = %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.certificate)
	if _, err := certificate.Verify(x509.VerifyOptions{
		Roots:       roots,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Fatalf("client certificate verification = %v", err)
	}
	if certificate.NotBefore != now.Add(-runtimeNodeClockSkew) || certificate.NotAfter != now.Add(cfg.ValidFor) {
		t.Fatalf("validity = %s..%s", certificate.NotBefore, certificate.NotAfter)
	}
	if bundle.Record.NodeID != nodeID || bundle.Record.DeviceCertificateSerial != strings.ToLower(certificate.SerialNumber.Text(16)) ||
		bundle.Record.NodeVersion != cfg.NodeVersion || bundle.Record.Capacity != cfg.Capacity ||
		!reflect.DeepEqual(bundle.Record.Features, runtime.RuntimeRequiredFeatures()) {
		t.Fatalf("registration record = %#v", bundle.Record)
	}
	spkiDigest := sha256.Sum256(certificate.RawSubjectPublicKeyInfo)
	if bundle.Record.DevicePublicKeyThumbprint != hex.EncodeToString(spkiDigest[:]) {
		t.Fatalf("SPKI thumbprint = %q", bundle.Record.DevicePublicKeyThumbprint)
	}
	key, err := parsePrivateKeyPEM(bundle.PrivateKeyPEM)
	if err != nil {
		t.Fatalf("parse issued private key: %v", err)
	}
	audit, err := auditRuntimeNodeCertificate(certificate, key, ca.certificate, now)
	if err != nil {
		t.Fatalf("audit issued certificate: %v", err)
	}
	if audit.NodeID != nodeID.String() || audit.PrivateKeyMatches == nil || !*audit.PrivateKeyMatches || !audit.CAValidationPerformed ||
		audit.RuntimeProtocolVersion != 2 || audit.RuntimeContractDigest != runtime.RuntimeContractDigest {
		t.Fatalf("audit = %#v", audit)
	}
}

func TestLoadRuntimeNodeCARejectsMismatchedOrIncapableCA(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	first := newRuntimeNodeTestCA(t, now)
	second := newRuntimeNodeTestCA(t, now)
	dir := t.TempDir()
	certFile := filepath.Join(dir, "ca.crt")
	keyFile := filepath.Join(dir, "ca.key")
	writeTestFile(t, certFile, first.certPEM, 0o644)
	writeTestFile(t, keyFile, second.keyPEM, 0o600)
	if _, _, err := loadRuntimeNodeCA(certFile, keyFile, now); err == nil || !strings.Contains(err.Error(), "do not match") {
		t.Fatalf("mismatched CA error = %v", err)
	}

	notCAKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	notCATemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(7),
		Subject:               pkix.Name{CommonName: "not-a-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	notCADER, err := x509.CreateCertificate(rand.Reader, notCATemplate, notCATemplate, &notCAKey.PublicKey, notCAKey)
	if err != nil {
		t.Fatal(err)
	}
	notCAKeyDER, err := x509.MarshalPKCS8PrivateKey(notCAKey)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: notCADER}), 0o644)
	writeTestFile(t, keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: notCAKeyDER}), 0o600)
	if _, _, err := loadRuntimeNodeCA(certFile, keyFile, now); err == nil || !strings.Contains(err.Error(), "must be a CA") {
		t.Fatalf("non-CA error = %v", err)
	}
}

func TestLoadRuntimeNodeCARejectsGroupReadablePrivateKey(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("Windows ACLs are not represented by Unix permission bits")
	}
	now := time.Now().UTC().Truncate(time.Second)
	ca := newRuntimeNodeTestCA(t, now)
	dir := t.TempDir()
	certFile := filepath.Join(dir, "ca.crt")
	keyFile := filepath.Join(dir, "ca.key")
	writeTestFile(t, certFile, ca.certPEM, 0o644)
	writeTestFile(t, keyFile, ca.keyPEM, 0o600)
	if err := os.Chmod(keyFile, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadRuntimeNodeCA(certFile, keyFile, now); err == nil || !strings.Contains(err.Error(), "permissions must be owner-only") {
		t.Fatalf("permissive CA key error = %v", err)
	}
}

func TestRuntimeNodeArtifactsPublishAtomicallyWithoutOverwrite(t *testing.T) {
	dir := t.TempDir()
	certOut := filepath.Join(dir, "node.crt")
	keyOut := filepath.Join(dir, "node.key")
	artifacts, err := stageRuntimeNodeArtifacts(certOut, []byte("certificate"), keyOut, []byte("private-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer artifacts.cleanupStaged()
	if err := artifacts.publish(); err != nil {
		t.Fatalf("publish() error = %v", err)
	}
	assertTestFile(t, certOut, "certificate", 0o644)
	assertTestFile(t, keyOut, "private-key", 0o600)
	if artifacts.certTemp != "" || artifacts.keyTemp != "" {
		t.Fatalf("staged files retained: %#v", artifacts)
	}

	secondCert := filepath.Join(dir, "second.crt")
	secondKey := filepath.Join(dir, "second.key")
	second, err := stageRuntimeNodeArtifacts(secondCert, []byte("new-cert"), secondKey, []byte("new-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer second.cleanupStaged()
	writeTestFile(t, secondCert, []byte("operator-file"), 0o644)
	if err := second.publish(); err == nil {
		t.Fatal("publish() overwrote a certificate created after staging")
	}
	if _, err := os.Lstat(secondKey); !os.IsNotExist(err) {
		t.Fatalf("partial key exists after failed publish: %v", err)
	}
	assertTestFile(t, secondCert, "operator-file", 0o644)
}

func TestRuntimeNodeArtifactsSyncSeparateOutputDirectories(t *testing.T) {
	certDir := t.TempDir()
	keyDir := t.TempDir()
	certOut := filepath.Join(certDir, "node.crt")
	keyOut := filepath.Join(keyDir, "node.key")
	artifacts, err := stageRuntimeNodeArtifacts(certOut, []byte("certificate"), keyOut, []byte("private-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer artifacts.cleanupStaged()
	if err := artifacts.publish(); err != nil {
		t.Fatalf("publish() across directories error = %v", err)
	}
	assertTestFile(t, certOut, "certificate", 0o644)
	assertTestFile(t, keyOut, "private-key", 0o600)
	if err := artifacts.cleanupPublished(); err != nil {
		t.Fatalf("cleanupPublished() across directories error = %v", err)
	}
	for _, path := range []string{certOut, keyOut} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("published artifact %s remains after cleanup: %v", path, err)
		}
	}
}

func TestIssueRuntimeNodeDatabaseFailureLeavesNoArtifacts(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	ca := newRuntimeNodeTestCA(t, now)
	dir := t.TempDir()
	caCertFile := filepath.Join(dir, "client-ca.crt")
	caKeyFile := filepath.Join(dir, "client-ca.key")
	certOut := filepath.Join(dir, "node.crt")
	keyOut := filepath.Join(dir, "node.key")
	writeTestFile(t, caCertFile, ca.certPEM, 0o644)
	writeTestFile(t, caKeyFile, ca.keyPEM, 0o600)

	_, err := issueRuntimeNode(t.Context(), runtimeNodeIssueConfig{
		DatabaseURL: "://invalid-database-url",
		CACertFile:  caCertFile,
		CAKeyFile:   caKeyFile,
		CertOut:     certOut,
		KeyOut:      keyOut,
		NodeID:      uuid.New(),
		DisplayName: "must roll back",
		NodeVersion: defaultRuntimeNodeVersion,
		Capacity:    1,
		ValidFor:    24 * time.Hour,
	}, func() time.Time { return now }, rand.Reader)
	if err == nil || !strings.Contains(err.Error(), "connect database") {
		t.Fatalf("issueRuntimeNode() error = %v", err)
	}
	for _, path := range []string{certOut, keyOut} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Fatalf("artifact %s exists after failure: %v", path, statErr)
		}
	}
	temps, err := filepath.Glob(filepath.Join(dir, ".node.*.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("staged artifacts remain after failure: %v", temps)
	}
}

func TestEnsureRuntimeNodeOutputsAbsentRejectsDanglingSymlink(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("symlink setup requires host permission on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "node.key")
	if err := os.Symlink(filepath.Join(dir, "missing"), path); err != nil {
		t.Fatal(err)
	}
	if err := ensureRuntimeNodeOutputsAbsent(path); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("dangling symlink error = %v", err)
	}
}

func TestInspectRuntimeNodeFilesVerifiesPairAndCA(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	ca := newRuntimeNodeTestCA(t, now)
	dir := t.TempDir()
	cfg := runtimeNodeIssueConfig{
		NodeID:      uuid.New(),
		DisplayName: "inspect",
		NodeVersion: defaultRuntimeNodeVersion,
		Capacity:    1,
		ValidFor:    24 * time.Hour,
	}
	bundle, err := createRuntimeNodeCertificate(cfg, ca.certificate, ca.privateKey, now, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certFile := filepath.Join(dir, "node.crt")
	keyFile := filepath.Join(dir, "node.key")
	caFile := filepath.Join(dir, "client-ca.crt")
	writeTestFile(t, certFile, bundle.CertificatePEM, 0o644)
	writeTestFile(t, keyFile, bundle.PrivateKeyPEM, 0o600)
	writeTestFile(t, caFile, ca.certPEM, 0o644)
	audit, err := inspectRuntimeNodeFiles(runtimeNodeInspectConfig{CertFile: certFile, KeyFile: keyFile, CACertFile: caFile}, now)
	if err != nil {
		t.Fatalf("inspectRuntimeNodeFiles() error = %v", err)
	}
	if audit.NodeID != cfg.NodeID.String() || audit.PrivateKeyMatches == nil || !*audit.PrivateKeyMatches || !audit.CAValidationPerformed {
		t.Fatalf("audit = %#v", audit)
	}

	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherDER, err := x509.MarshalPKCS8PrivateKey(otherKey)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: otherDER}), 0o600)
	if _, err := inspectRuntimeNodeFiles(runtimeNodeInspectConfig{CertFile: certFile, KeyFile: keyFile}, now); err == nil || !strings.Contains(err.Error(), "do not match") {
		t.Fatalf("mismatched key error = %v", err)
	}
}

func TestRunRuntimeNodeInspectOutputsStableJSON(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	ca := newRuntimeNodeTestCA(t, now)
	bundle, err := createRuntimeNodeCertificate(runtimeNodeIssueConfig{
		NodeID:      uuid.New(),
		DisplayName: "CLI inspect",
		NodeVersion: defaultRuntimeNodeVersion,
		Capacity:    1,
		ValidFor:    24 * time.Hour,
	}, ca.certificate, ca.privateKey, now, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile := filepath.Join(dir, "node.crt")
	keyFile := filepath.Join(dir, "node.key")
	writeTestFile(t, certFile, bundle.CertificatePEM, 0o644)
	writeTestFile(t, keyFile, bundle.PrivateKeyPEM, 0o600)
	var stdout, stderr bytes.Buffer
	if code := runRuntimeNode([]string{"inspect", "--cert", certFile, "--key", keyFile}, func(string) string { return "" }, &stdout, &stderr); code != 0 {
		t.Fatalf("runRuntimeNode() code=%d stderr=%s", code, stderr.String())
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode output: %v\n%s", err, stdout.String())
	}
	if output["node_id"] != bundle.Record.NodeID.String() || output["runtime_contract_digest"] != runtime.RuntimeContractDigest || output["private_key_matches"] != true {
		t.Fatalf("output = %#v", output)
	}
}

func TestRunRuntimeNodeUsageErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"inspect"}} {
		var stdout, stderr bytes.Buffer
		if code := runRuntimeNode(args, func(string) string { return "" }, &stdout, &stderr); code != 2 {
			t.Fatalf("runRuntimeNode(%v) code=%d, want 2", args, code)
		}
		if stderr.Len() == 0 {
			t.Fatalf("runRuntimeNode(%v) produced no error", args)
		}
	}
	for _, args := range [][]string{{"issue", "--help"}, {"inspect", "--help"}} {
		var stdout, stderr bytes.Buffer
		if code := runRuntimeNode(args, func(string) string { return "" }, &stdout, &stderr); code != 0 {
			t.Fatalf("runRuntimeNode(%v) code=%d stderr=%s", args, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "usage: api runtime-node") || stderr.Len() != 0 {
			t.Fatalf("runRuntimeNode(%v) stdout=%q stderr=%q", args, stdout.String(), stderr.String())
		}
	}
}

func TestRandomRuntimeNodeSerialHas128BitsOfEntropy(t *testing.T) {
	serial, err := randomRuntimeNodeSerial(bytes.NewReader(bytes.Repeat([]byte{0xa5}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if serial.Sign() <= 0 || serial.BitLen() != 129 {
		t.Fatalf("serial = %s (%d bits)", serial, serial.BitLen())
	}
}

func newRuntimeNodeTestCA(t *testing.T, now time.Time) runtimeNodeTestCA {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKeyDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	subjectKeyID := sha256.Sum256(publicKeyDER)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "OpenLinker Runtime Node Test CA"},
		NotBefore:             now.Add(-24 * time.Hour),
		NotAfter:              now.Add(2 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		SubjectKeyId:          subjectKeyID[:],
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return runtimeNodeTestCA{
		certificate: certificate,
		privateKey:  privateKey,
		certPEM:     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:      pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}
}

func writeTestFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

func assertTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != content {
		t.Fatalf("%s = %q, want %q", path, data, content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != mode {
		t.Fatalf("%s mode = %o, want %o", path, info.Mode().Perm(), mode)
	}
}
