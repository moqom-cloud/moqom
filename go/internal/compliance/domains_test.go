package compliance

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func hardened(domain string) DomainDNS {
	return DomainDNS{Domain: domain, Signed: true, CAA: []CAARecord{
		{Tag: "issue", Value: "pki.goog"},
		{Tag: "issue", Value: "letsencrypt.org"},
		{Tag: "issuewild", Value: ";"},
	}}
}

func domainFindings(e Evidence) []Finding {
	var out []Finding
	for _, f := range Evaluate(e, DefaultPolicy(), now) {
		if strings.HasPrefix(f.Rule, "DNS-") || strings.HasPrefix(f.Rule, "CT-") {
			out = append(out, f)
		}
	}
	return out
}

func TestHardenedDomainHasNoFindings(t *testing.T) {
	e := clean()
	e.DomainDNS = []DomainDNS{hardened("moqom.cloud")}
	e.OwnCertificates = []OwnCertificate{{Name: "sites", PubkeySHA256: "aa"}}
	e.Issuances = []Issuance{{ID: "1", DNSNames: []string{"moqom.cloud"}, PubkeySHA256: "aa", NotBefore: now.Add(-time.Hour)}}
	if got := domainFindings(e); len(got) != 0 {
		t.Fatalf("findings on a hardened domain: %+v", got)
	}
}

func TestMissingCAAAndDNSSEC(t *testing.T) {
	e := clean()
	e.DomainDNS = []DomainDNS{{Domain: "moqom.dev"}}
	got := domainFindings(e)
	if len(got) != 2 || got[0].Rule != "DNS-001" || got[0].Severity != SeverityHigh || got[1].Rule != "DNS-003" {
		t.Fatalf("got %+v", got)
	}
}

func TestCAAThatAllowsAnotherIssuerOrWildcards(t *testing.T) {
	e := clean()
	e.DomainDNS = []DomainDNS{{Domain: "moqom.dev", Signed: true, CAA: []CAARecord{
		{Tag: "issue", Value: "pki.goog"},
		{Tag: "issue", Value: "sectigo.com; validationmethods=dns-01"},
	}}}
	got := domainFindings(e)
	if len(got) != 1 || got[0].Rule != "DNS-002" || !strings.Contains(got[0].Detail, "sectigo.com") || !strings.Contains(got[0].Detail, "issuewild") {
		t.Fatalf("got %+v", got)
	}
}

func TestUnknownCertificateIsCritical(t *testing.T) {
	e := clean()
	e.OwnCertificates = []OwnCertificate{{Name: "sites", PubkeySHA256: "aa"}}
	e.Issuances = []Issuance{
		{ID: "7", Issuer: "Let's Encrypt", DNSNames: []string{"moqom.cloud"}, PubkeySHA256: "bb", NotBefore: now.Add(-2 * time.Hour)},
		// Logged twice, once per domain searched: one finding.
		{ID: "7", Issuer: "Let's Encrypt", DNSNames: []string{"moqom.cloud"}, PubkeySHA256: "bb", NotBefore: now.Add(-2 * time.Hour)},
		// Old news, and revoked: neither is reported.
		{ID: "8", PubkeySHA256: "cc", NotBefore: now.Add(-30 * 24 * time.Hour)},
		{ID: "9", PubkeySHA256: "dd", NotBefore: now.Add(-time.Hour), Revoked: true},
	}
	got := domainFindings(e)
	if len(got) != 1 || got[0].Rule != "CT-001" || got[0].Severity != SeverityCritical || got[0].Resource != "certificate/7" {
		t.Fatalf("got %+v", got)
	}

	p := DefaultPolicy()
	p.KnownPublicKeys = []string{"BB"}
	for _, f := range Evaluate(e, p, now) {
		if f.Rule == "CT-001" {
			t.Fatalf("a known key was reported: %+v", f)
		}
	}
}

func selfSigned(t *testing.T, name string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := x509.ParseCertificate(der)
	sum := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), hex.EncodeToString(sum[:])
}

func TestCollectDomains(t *testing.T) {
	certificate, keyHash := selfSigned(t, "moqom.cloud")

	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("credentials sent to %s", r.URL.Path)
		}
		switch r.URL.Path {
		case "/doh":
			name, kind := r.URL.Query().Get("name"), r.URL.Query().Get("type")
			switch {
			case name == "moqom.cloud" && kind == "CAA":
				fmt.Fprint(w, `{"Status":0,"Answer":[{"type":5,"data":"elsewhere."},{"type":257,"data":"0 issue \"pki.goog\""},{"type":257,"data":"0 issuewild \";\""}]}`)
			case name == "moqom.cloud" && kind == "DS":
				fmt.Fprint(w, `{"Status":0,"Answer":[{"type":43,"data":"2371 13 2 abcd"}]}`)
			default:
				fmt.Fprint(w, `{"Status":0}`)
			}
		case "/certspotter/issuances":
			if r.URL.Query().Get("after") != "" {
				fmt.Fprint(w, `[]`)
				return
			}
			body, _ := json.Marshal([]map[string]any{
				{"id": "1", "dns_names": []string{"moqom.cloud"}, "pubkey_sha256": strings.ToUpper(keyHash), "not_before": now.Add(-time.Hour), "issuer": map[string]string{"friendly_name": "Google Trust Services"}},
			})
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer public.Close()

	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/compute/projects/p/aggregated/sslCertificates" {
			http.NotFound(w, r)
			return
		}
		body, _ := json.Marshal(map[string]any{"items": map[string]any{
			"global": map[string]any{"sslCertificates": []map[string]string{
				{"name": "sites", "certificate": certificate},
				{"name": "provisioning"},
			}},
		}})
		w.Write(body)
	}))
	defer google.Close()

	c := &Collector{
		Client: google.Client(), Project: "p", Public: public.Client(),
		Domains: []string{"moqom.cloud"},
		Bases:   map[string]string{"compute": google.URL + "/compute", "doh": public.URL + "/doh", "certspotter": public.URL + "/certspotter"},
	}
	var e Evidence
	ctx := context.Background()
	for name, run := range map[string]func(context.Context, *Evidence) error{
		"dns": c.domainDNS, "ct": c.issuances, "certs": c.ownCertificates,
	} {
		if err := run(ctx, &e); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if len(e.DomainDNS) != 1 || !e.DomainDNS[0].Signed || len(e.DomainDNS[0].CAA) != 2 {
		t.Fatalf("dns: %+v", e.DomainDNS)
	}
	if len(e.OwnCertificates) != 1 || e.OwnCertificates[0].PubkeySHA256 != keyHash {
		t.Fatalf("certificates: %+v", e.OwnCertificates)
	}
	e.CollectedAt = now
	got := domainFindings(e)
	if len(got) != 0 {
		t.Fatalf("findings on a hardened domain serving its own certificate: %+v", got)
	}
}

func TestResolveRefusesServfail(t *testing.T) {
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"Status":2}`)
	}))
	defer public.Close()
	c := &Collector{Public: public.Client(), Domains: []string{"moqom.cloud"}, Bases: map[string]string{"doh": public.URL}}
	var e Evidence
	if err := c.domainDNS(context.Background(), &e); err == nil || len(e.DomainDNS) != 0 {
		t.Fatalf("SERVFAIL read as an answer: %v %+v", err, e.DomainDNS)
	}
}
