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
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"neutrino/internal/certutil"
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
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if _, err := a.store.GetNode(r.Context(), nodeID); err != nil {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	if err := a.store.ValidateNodeEnrollCode(r.Context(), nodeID, req.EnrollCode); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	csr, err := certutil.ParseCSRPEM(req.CSRPEM)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	wantCN := fmt.Sprintf("node-%d", nodeID)
	if strings.TrimSpace(csr.Subject.CommonName) != wantCN {
		http.Error(w, "csr CN must be "+wantCN, http.StatusBadRequest)
		return
	}

	caCert, caKey, err := a.loadSigningCA()
	if err != nil {
		http.Error(w, "signing CA not configured", http.StatusInternalServerError)
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
		http.Error(w, "sign failed", http.StatusInternalServerError)
		return
	}
	// Keep allowlist tight: install the new cert, consume the one-time code, and
	// revoke any previously active certs atomically.
	revoked, err := a.store.CompleteNodeEnroll(r.Context(), nodeID, req.EnrollCode, cert, now)
	if err != nil {
		if strings.Contains(err.Error(), "enroll_code") {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		http.Error(w, "enroll failed", http.StatusInternalServerError)
		return
	}
	_ = a.store.InsertAuditLog(r.Context(), "node", fmt.Sprintf("%d", nodeID), "node.enroll", "node", fmt.Sprintf("%d", nodeID),
		fmt.Sprintf(`{"ok":true,"cert_sha256":"%s","revoked_old":%d}`, certutil.CertSHA256Hex(cert), revoked),
		a.clientIPFromRequest(r), r.UserAgent(), requestIDFromContext(r.Context()))
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
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req struct {
		CSRPEM string `json:"csr_pem"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	csr, err := certutil.ParseCSRPEM(req.CSRPEM)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	wantCN := fmt.Sprintf("node-%d", nodeID)
	if strings.TrimSpace(csr.Subject.CommonName) != wantCN {
		http.Error(w, "csr CN must be "+wantCN, http.StatusBadRequest)
		return
	}
	caCert, caKey, err := a.loadSigningCA()
	if err != nil {
		http.Error(w, "signing CA not configured", http.StatusInternalServerError)
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
		http.Error(w, "sign failed", http.StatusInternalServerError)
		return
	}
	revoked, err := a.store.RenewNodeCertPin(r.Context(), nodeID, "agent_client", cert, now)
	if err != nil {
		http.Error(w, "renew failed", http.StatusInternalServerError)
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
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
	} else {
		// Allow simple HTML forms (safer for admin UI); but require an explicit action.
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		req.CertSHA256 = strings.TrimSpace(r.FormValue("cert_sha256"))
		req.Reason = strings.TrimSpace(r.FormValue("reason"))
		revokeAll = strings.TrimSpace(r.FormValue("revoke_all")) == "1"
	}
	if !revokeAll && strings.TrimSpace(req.CertSHA256) == "" {
		http.Error(w, "cert_sha256 required (or set revoke_all=1)", http.StatusBadRequest)
		return
	}
	if revokeAll {
		req.CertSHA256 = ""
	}

	n, err := a.store.RevokeNodeCertPin(r.Context(), nodeID, req.CertSHA256, req.Reason)
	if err != nil {
		http.Error(w, "revoke failed", http.StatusBadRequest)
		return
	}
	if act := actorFromRequest(a, r); act.typ != "" {
		detail, _ := json.Marshal(map[string]any{"cert_sha256": strings.TrimSpace(req.CertSHA256), "reason": strings.TrimSpace(req.Reason), "revoked": n})
		_ = a.store.InsertAuditLog(r.Context(), act.typ, act.id, "node.cert.revoke", "node", fmt.Sprintf("%d", nodeID), string(detail), a.clientIPFromRequest(r), r.UserAgent(), requestIDFromContext(r.Context()))
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "revoked": n})
}
