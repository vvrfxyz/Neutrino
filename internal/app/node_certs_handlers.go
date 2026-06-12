package app

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"neutrino/internal/certutil"
	"neutrino/internal/repo"
)

type signedCertResponse struct {
	CABundlePEM string `json:"ca_bundle_pem"`
	CertPEM     string `json:"cert_pem"`
	NotAfter    string `json:"not_after"`
	SPKISHA256  string `json:"spki_sha256"`
	CertSHA256  string `json:"cert_sha256"`
}

func (a *App) loadSigningCA() (*x509.Certificate, crypto.Signer, error) {
	caPath := strings.TrimSpace(a.cfg.PanelAgentMTLSSigningCACertPath)
	keyPath := strings.TrimSpace(a.cfg.PanelAgentMTLSSigningCAKeyPath)
	if caPath == "" || keyPath == "" {
		return nil, nil, fmt.Errorf("signing CA not configured")
	}
	if a.cfg.PanelAgentMTLSSigningRequireStrictKeyPerms {
		if st, err := os.Stat(keyPath); err == nil {
			// Refuse group/world-readable keys by default.
			if st.Mode().Perm()&0o077 != 0 {
				return nil, nil, fmt.Errorf("signing CA key perms too open: %s (expected 0600/0400)", st.Mode().Perm().String())
			}
		}
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, nil, err
	}
	block, _ := pem.Decode(caPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("invalid signing CA cert pem")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	if !caCert.IsCA {
		return nil, nil, fmt.Errorf("signing CA cert is not a CA")
	}
	if a.cfg.PanelAgentMTLSSigningRequireIntermediate {
		// Self-signed CA implies the root key is resident on the panel. For production, require an intermediate.
		if caCert.Subject.String() == caCert.Issuer.String() && caCert.CheckSignatureFrom(caCert) == nil {
			return nil, nil, fmt.Errorf("signing CA appears to be self-signed; require intermediate CA (set PANEL_AGENT_MTLS_SIGNING_REQUIRE_INTERMEDIATE=false to override)")
		}
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, nil, fmt.Errorf("invalid signing CA key pem")
	}
	var key any
	switch kb.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(kb.Bytes)
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(kb.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(kb.Bytes)
	default:
		return nil, nil, fmt.Errorf("unsupported key type %q", kb.Type)
	}
	if err != nil {
		return nil, nil, err
	}
	switch k := key.(type) {
	case *rsa.PrivateKey:
		return caCert, k, nil
	case *ecdsa.PrivateKey:
		return caCert, k, nil
	case ed25519.PrivateKey:
		return caCert, k, nil
	default:
		return nil, nil, fmt.Errorf("unsupported key")
	}
}

func (a *App) caBundlePEM() string {
	paths := strings.TrimSpace(a.cfg.PanelAgentMTLSCACertPaths)
	if paths == "" && strings.TrimSpace(a.cfg.PanelAgentMTLSSigningCACertPath) != "" {
		paths = a.cfg.PanelAgentMTLSSigningCACertPath
	}
	var b strings.Builder
	for _, p := range strings.Split(paths, ",") {
		pp := strings.TrimSpace(p)
		if pp == "" {
			continue
		}
		raw, err := os.ReadFile(pp)
		if err != nil {
			continue
		}
		b.Write(raw)
		if len(raw) > 0 && raw[len(raw)-1] != '\n' {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func (a *App) handleAPINodeEnroll(w http.ResponseWriter, r *http.Request, nodeID int64) {
	var req struct {
		EnrollCode string `json:"enroll_code"`
		CSRPEM     string `json:"csr_pem"`
	}
	body, err := readAllLimit(r.Body, 64*1024)
	if err != nil {
		a.apiError(w, http.StatusBadRequest, "invalid body")
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		a.apiError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if _, err := a.nodes().Get(r.Context(), nodeID); err != nil {
		a.apiError(w, http.StatusNotFound, "node not found")
		return
	}
	if err := a.nodes().ValidateEnrollCode(r.Context(), nodeID, req.EnrollCode); err != nil {
		a.apiError(w, http.StatusForbidden, err.Error())
		return
	}
	csr, err := certutil.ParseCSRPEM(req.CSRPEM)
	if err != nil {
		a.apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	wantCN := fmt.Sprintf("node-%d", nodeID)
	if strings.TrimSpace(csr.Subject.CommonName) != wantCN {
		a.apiError(w, http.StatusBadRequest, "csr CN must be "+wantCN)
		return
	}

	caCert, caKey, err := a.loadSigningCA()
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, "signing CA not configured")
		return
	}
	now := time.Now().UTC()
	cert, err := certutil.SignCSR(certutil.SignCSRInput{
		CACert:      caCert,
		CAKey:       caKey,
		CSR:         csr,
		NotBefore:   now.Add(-5 * time.Minute),
		NotAfter:    now.Add(30 * 24 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		KeyUsage:    x509.KeyUsageDigitalSignature,
	})
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, "sign failed")
		return
	}
	// Keep allowlist tight: install the new cert, consume the one-time code, and
	// revoke any previously active certs atomically.
	revoked, err := a.nodes().CompleteEnroll(r.Context(), nodeID, req.EnrollCode, cert, now)
	if err != nil {
		if errors.Is(err, repo.ErrEnrollCodeInvalid) {
			a.apiError(w, http.StatusForbidden, err.Error())
			return
		}
		a.apiError(w, http.StatusInternalServerError, "enroll failed")
		return
	}
	auditActionAs(a, r, "node", fmt.Sprintf("%d", nodeID), "node.enroll", "node", fmt.Sprintf("%d", nodeID),
		map[string]any{"ok": true, "cert_sha256": certutil.CertSHA256Hex(cert), "revoked_old": revoked})
	resp := signedCertResponse{
		CABundlePEM: a.caBundlePEM(),
		CertPEM:     certutil.CertToPEM(cert),
		NotAfter:    cert.NotAfter.UTC().Format(time.RFC3339),
		SPKISHA256:  certutil.SPKISHA256Hex(cert),
		CertSHA256:  certutil.CertSHA256Hex(cert),
	}
	a.writeJSON(w, http.StatusOK, resp)
}

func (a *App) handleAPINodeCertRenew(w http.ResponseWriter, r *http.Request, nodeID int64) {
	// Node-agent control-plane is mTLS only (no bearer tokens).
	if mtlsID, ok := nodeIDFromMTLS(r); !ok || mtlsID != nodeID {
		a.apiError(w, http.StatusForbidden, "forbidden")
		return
	}
	var req struct {
		CSRPEM string `json:"csr_pem"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.apiError(w, http.StatusBadRequest, "invalid json")
		return
	}
	csr, err := certutil.ParseCSRPEM(req.CSRPEM)
	if err != nil {
		a.apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	wantCN := fmt.Sprintf("node-%d", nodeID)
	if strings.TrimSpace(csr.Subject.CommonName) != wantCN {
		a.apiError(w, http.StatusBadRequest, "csr CN must be "+wantCN)
		return
	}
	caCert, caKey, err := a.loadSigningCA()
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, "signing CA not configured")
		return
	}
	now := time.Now().UTC()
	cert, err := certutil.SignCSR(certutil.SignCSRInput{
		CACert:      caCert,
		CAKey:       caKey,
		CSR:         csr,
		NotBefore:   now.Add(-5 * time.Minute),
		NotAfter:    now.Add(30 * 24 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		KeyUsage:    x509.KeyUsageDigitalSignature,
	})
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, "sign failed")
		return
	}
	revoked, err := a.nodes().RenewCertPin(r.Context(), nodeID, "agent_client", cert, now)
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, "renew failed")
		return
	}
	auditAction(a, r, "node.cert.renew", "node", fmt.Sprintf("%d", nodeID), map[string]any{"cert_sha256": certutil.CertSHA256Hex(cert), "revoked_old": revoked})
	resp := signedCertResponse{
		CABundlePEM: a.caBundlePEM(),
		CertPEM:     certutil.CertToPEM(cert),
		NotAfter:    cert.NotAfter.UTC().Format(time.RFC3339),
		SPKISHA256:  certutil.SPKISHA256Hex(cert),
		CertSHA256:  certutil.CertSHA256Hex(cert),
	}
	a.writeJSON(w, http.StatusOK, resp)
}

func (a *App) handleAPINodeCertRevoke(w http.ResponseWriter, r *http.Request, nodeID int64) {
	var req struct {
		CertSHA256 string `json:"cert_sha256"`
		Reason     string `json:"reason"`
	}
	revokeAll := false
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if strings.HasPrefix(ct, "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			a.apiError(w, http.StatusBadRequest, "invalid json")
			return
		}
	} else {
		// Allow simple HTML forms (safer for admin UI); but require an explicit action.
		if err := r.ParseForm(); err != nil {
			a.apiError(w, http.StatusBadRequest, "bad form")
			return
		}
		req.CertSHA256 = strings.TrimSpace(r.FormValue("cert_sha256"))
		req.Reason = strings.TrimSpace(r.FormValue("reason"))
		revokeAll = strings.TrimSpace(r.FormValue("revoke_all")) == "1"
	}
	if !revokeAll && strings.TrimSpace(req.CertSHA256) == "" {
		a.apiError(w, http.StatusBadRequest, "cert_sha256 required (or set revoke_all=1)")
		return
	}
	if revokeAll {
		req.CertSHA256 = ""
	}

	n, err := a.nodes().RevokeCertPin(r.Context(), nodeID, req.CertSHA256, req.Reason)
	if err != nil {
		a.apiError(w, http.StatusBadRequest, "revoke failed")
		return
	}
	auditAction(a, r, "node.cert.revoke", "node", fmt.Sprintf("%d", nodeID), map[string]any{"cert_sha256": strings.TrimSpace(req.CertSHA256), "reason": strings.TrimSpace(req.Reason), "revoked": n})
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "revoked": n})
}
