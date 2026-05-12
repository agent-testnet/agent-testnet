package controlplane

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CA manages the testnet root certificate authority.
type CA struct {
	mu       sync.Mutex
	rootKey  *ecdsa.PrivateKey
	rootCert *x509.Certificate
	rootPEM  []byte
	dataDir  string
	certDir  string
}

func NewCA(dataDir, keyFile, certFile string) (*CA, error) {
	ca := &CA{
		dataDir: dataDir,
		certDir: filepath.Join(dataDir, "certs"),
	}
	if err := os.MkdirAll(ca.certDir, 0o700); err != nil {
		return nil, err
	}

	if keyFile == "" {
		keyFile = filepath.Join(dataDir, "ca-key.pem")
	}
	if certFile == "" {
		certFile = filepath.Join(dataDir, "ca-cert.pem")
	}

	keyData, errK := os.ReadFile(keyFile)
	certData, errC := os.ReadFile(certFile)

	if errK == nil && errC == nil {
		return ca, ca.loadFromPEM(keyData, certData)
	}

	if err := ca.generate(); err != nil {
		return nil, err
	}
	if err := ca.persist(keyFile, certFile); err != nil {
		return nil, err
	}
	return ca, nil
}

func (ca *CA) loadFromPEM(keyPEM, certPEM []byte) error {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return fmt.Errorf("failed to decode CA private key PEM")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse CA key: %w", err)
	}

	block, _ = pem.Decode(certPEM)
	if block == nil {
		return fmt.Errorf("failed to decode CA cert PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse CA cert: %w", err)
	}

	ca.rootKey = key
	ca.rootCert = cert
	ca.rootPEM = certPEM
	return nil
}

func (ca *CA) generate() error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate CA key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Testnet"},
			CommonName:   "Testnet Root CA",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create CA cert: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return err
	}

	ca.rootKey = key
	ca.rootCert = cert
	ca.rootPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	return nil
}

func (ca *CA) persist(keyFile, certFile string) error {
	keyDER, err := x509.MarshalECPrivateKey(ca.rootKey)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(certFile), 0o700); err != nil {
		return err
	}
	return os.WriteFile(certFile, ca.rootPEM, 0o644)
}

// RootCertPEM returns the root CA certificate in PEM format.
func (ca *CA) RootCertPEM() []byte {
	return ca.rootPEM
}

// IssueCert generates a leaf certificate signed by the CA for the given node.
// SANs include the auto-name ({name}.testnet) plus all declared domains.
func (ca *CA) IssueCert(nodeName string, domains []string) (certPEM, keyPEM []byte, err error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	nodeDir := filepath.Join(ca.certDir, nodeName)
	certPath := filepath.Join(nodeDir, "cert.pem")
	keyPath := filepath.Join(nodeDir, "key.pem")

	// Return cached certs if they exist
	if cData, err := os.ReadFile(certPath); err == nil {
		if kData, err := os.ReadFile(keyPath); err == nil {
			return cData, kData, nil
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate leaf key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}

	sans := []string{nodeName + ".testnet"}
	sans = append(sans, domains...)

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: nodeName + ".testnet",
		},
		DNSNames:  sans,
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, ca.rootCert, &key.PublicKey, ca.rootKey)
	if err != nil {
		return nil, nil, fmt.Errorf("sign leaf cert: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.MkdirAll(nodeDir, 0o700); err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, nil, err
	}

	return certPEM, keyPEM, nil
}

// GenerateAPICert creates a self-signed TLS cert for the control plane API.
func (ca *CA) GenerateAPICert(certFile, keyFile string) error {
	if _, err := os.Stat(certFile); err == nil {
		return nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: "testnet-server",
		},
		DNSNames:  []string{"localhost", "testnet-server"},
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, ca.rootCert, &key.PublicKey, ca.rootKey)
	if err != nil {
		return err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	kPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.MkdirAll(filepath.Dir(certFile), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(certFile, certPEM, 0o644); err != nil {
		return err
	}
	return os.WriteFile(keyFile, kPEM, 0o600)
}
