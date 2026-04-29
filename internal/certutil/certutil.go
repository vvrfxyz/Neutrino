package certutil

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"time"
)

func SPKISHA256Hex(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

func CertSHA256Hex(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

func SerialHex(cert *x509.Certificate) string {
	if cert == nil || cert.SerialNumber == nil {
		return ""
	}
	// Keep it stable and comparable.
	return strings.TrimLeft(strings.ToLower(cert.SerialNumber.Text(16)), "0")
}

func ParseCSRPEM(csrPEM string) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil {
		return nil, fmt.Errorf("invalid csr_pem")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("csr signature: %w", err)
	}
	return csr, nil
}

func NewSerialNumber() (*big.Int, error) {
	// 128-bit serial.
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}

type SignCSRInput struct {
	CACert *x509.Certificate
	CAKey  any // crypto.Signer

	CSR *x509.CertificateRequest

	NotBefore time.Time
	NotAfter  time.Time

	ExtKeyUsage []x509.ExtKeyUsage
	KeyUsage    x509.KeyUsage
}

func SignCSR(in SignCSRInput) (*x509.Certificate, error) {
	if in.CACert == nil || in.CSR == nil || in.CAKey == nil {
		return nil, fmt.Errorf("missing ca/csr/key")
	}
	serial, err := NewSerialNumber()
	if err != nil {
		return nil, err
	}
	if in.NotBefore.IsZero() {
		in.NotBefore = time.Now().UTC().Add(-5 * time.Minute)
	}
	if in.NotAfter.IsZero() {
		in.NotAfter = time.Now().UTC().Add(30 * 24 * time.Hour)
	}

	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      in.CSR.Subject,
		NotBefore:    in.NotBefore,
		NotAfter:     in.NotAfter,
		KeyUsage:     in.KeyUsage,
		ExtKeyUsage:  in.ExtKeyUsage,
		// No SANs needed for pure client certs.
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tpl, in.CACert, in.CSR.PublicKey, in.CAKey)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

func CertToPEM(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
}

