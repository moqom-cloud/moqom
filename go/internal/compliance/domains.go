package compliance

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The public domains are checked from outside, the way an attacker who has
// taken a registry sees them: what DNS says about who may issue, whether the
// delegation is signed, and what Certificate Transparency says was issued.
//
// Those two services are reached with Collector.Public, never Collector.Client:
// Client carries the job's Google credentials, and neither DNS-over-HTTPS nor a
// CT search API has any business receiving them.

func init() {
	DefaultBases["doh"] = "https://dns.google/resolve"
	DefaultBases["certspotter"] = "https://api.certspotter.com/v1"
}

// DomainDNS is what public DNS says about one domain.
type DomainDNS struct {
	Domain string      `json:"domain"`
	CAA    []CAARecord `json:"caa"`
	// Signed is whether the parent zone holds a DS record for the domain.
	Signed bool `json:"dnssec_signed"`
}

// CAARecord is one CAA record (RFC 8659).
type CAARecord struct {
	Flags int    `json:"flags"`
	Tag   string `json:"tag"`
	Value string `json:"value"`
}

// Issuance is one certificate Certificate Transparency logged for a domain or
// a name beneath it.
type Issuance struct {
	Domain       string    `json:"domain"`
	ID           string    `json:"id"`
	DNSNames     []string  `json:"dns_names"`
	Issuer       string    `json:"issuer"`
	PubkeySHA256 string    `json:"pubkey_sha256"`
	NotBefore    time.Time `json:"not_before"`
	Revoked      bool      `json:"revoked"`
}

// OwnCertificate is a certificate this project's load balancers serve. CT
// entries whose key is not one of these were not requested by us.
type OwnCertificate struct {
	Name         string   `json:"name"`
	DNSNames     []string `json:"dns_names"`
	PubkeySHA256 string   `json:"pubkey_sha256"`
}

func (c *Collector) public() *http.Client {
	if c.Public != nil {
		return c.Public
	}
	return &http.Client{Timeout: time.Minute}
}

func (c *Collector) getPublic(ctx context.Context, api, path string, query url.Values, header http.Header, into any) error {
	target := c.base(api) + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	for k, v := range header {
		request.Header[k] = v
	}
	return send(c.public(), request, into)
}

func (c *Collector) domainDNS(ctx context.Context, e *Evidence) error {
	e.DomainDNS = []DomainDNS{}
	var failures []string
	for _, domain := range c.Domains {
		record := DomainDNS{Domain: domain, CAA: []CAARecord{}}
		caa, err := c.resolve(ctx, domain, "CAA")
		if err == nil {
			for _, data := range caa {
				if r, ok := parseCAA(data); ok {
					record.CAA = append(record.CAA, r)
				}
			}
			var ds []string
			ds, err = c.resolve(ctx, domain, "DS")
			record.Signed = len(ds) > 0
		}
		if err != nil {
			failures = append(failures, domain+": "+err.Error())
			continue
		}
		e.DomainDNS = append(e.DomainDNS, record)
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

// resolve returns the data of each answer of the given type, through Google
// Public DNS's JSON API. An answer of another type (a CNAME followed on the
// way) is skipped.
func (c *Collector) resolve(ctx context.Context, name, rrtype string) ([]string, error) {
	types := map[string]int{"CAA": 257, "DS": 43}
	var reply struct {
		Status int `json:"Status"`
		Answer []struct {
			Type int    `json:"type"`
			Data string `json:"data"`
		} `json:"Answer"`
	}
	if err := c.getPublic(ctx, "doh", "", url.Values{"name": {name}, "type": {rrtype}}, nil, &reply); err != nil {
		return nil, err
	}
	// NOERROR and NXDOMAIN are answers; anything else (SERVFAIL, which is what
	// a broken DNSSEC chain produces) is not, and must not read as "no CAA".
	if reply.Status != 0 && reply.Status != 3 {
		return nil, fmt.Errorf("%s %s: DNS status %d", name, rrtype, reply.Status)
	}
	var out []string
	for _, a := range reply.Answer {
		if a.Type == types[rrtype] {
			out = append(out, a.Data)
		}
	}
	return out, nil
}

// parseCAA reads presentation form: `0 issue "pki.goog"`.
func parseCAA(data string) (CAARecord, bool) {
	fields := strings.SplitN(strings.TrimSpace(data), " ", 3)
	if len(fields) != 3 {
		return CAARecord{}, false
	}
	flags, err := strconv.Atoi(fields[0])
	if err != nil {
		return CAARecord{}, false
	}
	return CAARecord{Flags: flags, Tag: strings.ToLower(fields[1]), Value: strings.Trim(fields[2], `"`)}, true
}

func (c *Collector) issuances(ctx context.Context, e *Evidence) error {
	e.Issuances = []Issuance{}
	var header http.Header
	if c.CertSpotterToken != "" {
		header = http.Header{"Authorization": {"Bearer " + c.CertSpotterToken}}
	}
	for _, domain := range c.Domains {
		query := url.Values{
			"domain":             {domain},
			"include_subdomains": {"true"},
			"expand":             {"dns_names", "issuer", "revocation"},
		}
		for page := 0; page < 100; page++ {
			var batch []struct {
				ID           string    `json:"id"`
				DNSNames     []string  `json:"dns_names"`
				PubkeySHA256 string    `json:"pubkey_sha256"`
				NotBefore    time.Time `json:"not_before"`
				Issuer       struct {
					FriendlyName string `json:"friendly_name"`
					Name         string `json:"name"`
				} `json:"issuer"`
				Revocation struct {
					Revoked bool `json:"revoked"`
				} `json:"revocation"`
			}
			if err := c.getPublic(ctx, "certspotter", "/issuances", query, header, &batch); err != nil {
				return fmt.Errorf("%s: %w", domain, err)
			}
			if len(batch) == 0 {
				break
			}
			for _, b := range batch {
				issuer := b.Issuer.FriendlyName
				if issuer == "" {
					issuer = b.Issuer.Name
				}
				e.Issuances = append(e.Issuances, Issuance{
					Domain: domain, ID: b.ID, DNSNames: b.DNSNames, Issuer: issuer,
					PubkeySHA256: strings.ToLower(b.PubkeySHA256), NotBefore: b.NotBefore.UTC(), Revoked: b.Revocation.Revoked,
				})
			}
			query.Set("after", batch[len(batch)-1].ID)
		}
	}
	return nil
}

// ownCertificates lists every SSL certificate in the project, global and
// regional, with the key it carries. A managed certificate still provisioning
// has no certificate yet and contributes nothing.
func (c *Collector) ownCertificates(ctx context.Context, e *Evidence) error {
	e.OwnCertificates = []OwnCertificate{}
	return c.list(ctx, "compute", c.projectPath()+"/aggregated/sslCertificates", nil, func(raw []byte) (string, error) {
		var page struct {
			Items map[string]struct {
				SSLCertificates []struct {
					Name        string `json:"name"`
					Certificate string `json:"certificate"`
				} `json:"sslCertificates"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		for _, scope := range page.Items {
			for _, cert := range scope.SSLCertificates {
				block, _ := pem.Decode([]byte(cert.Certificate))
				if block == nil {
					continue
				}
				parsed, err := x509.ParseCertificate(block.Bytes)
				if err != nil {
					return "", fmt.Errorf("sslCertificate %s: %w", cert.Name, err)
				}
				sum := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
				e.OwnCertificates = append(e.OwnCertificates, OwnCertificate{
					Name: cert.Name, DNSNames: parsed.DNSNames, PubkeySHA256: hex.EncodeToString(sum[:]),
				})
			}
		}
		return page.NextPageToken, nil
	})
}

func domainRules(e Evidence, p Policy, now time.Time, add func(Finding)) {
	allowed := set(p.CAAIssuers)
	for _, d := range e.DomainDNS {
		resource := "domain/" + d.Domain
		var issuers []string
		forbidsWildcards := false
		for _, r := range d.CAA {
			switch r.Tag {
			case "issue":
				issuers = append(issuers, caaIssuer(r.Value))
			case "issuewild":
				forbidsWildcards = forbidsWildcards || caaIssuer(r.Value) == ""
			}
		}
		if len(issuers) == 0 {
			add(Finding{
				Rule: "DNS-001", Severity: SeverityHigh, Resource: resource,
				Title:    "No CAA records",
				Detail:   "Any certificate authority may issue for this domain. Publish CAA issue records naming only " + strings.Join(p.CAAIssuers, " and ") + `, and issuewild ";".`,
				Controls: []string{"SOC2:CC6.7", "ISO27001:A.8.20", "ISO27001:A.8.24"},
			})
		} else {
			var extra []string
			for _, issuer := range issuers {
				if issuer != "" && !allowed[issuer] {
					extra = append(extra, issuer)
				}
			}
			if len(extra) > 0 || !forbidsWildcards {
				detail := ""
				if len(extra) > 0 {
					detail = "CAA permits " + strings.Join(extra, ", ") + ", which the baseline does not. "
				}
				if !forbidsWildcards {
					detail += `No issuewild ";", so wildcard certificates may be issued.`
				}
				add(Finding{
					Rule: "DNS-002", Severity: SeverityMedium, Resource: resource,
					Title: "CAA broader than the baseline", Detail: strings.TrimSpace(detail),
					Controls: []string{"SOC2:CC6.7", "ISO27001:A.8.24"},
				})
			}
		}
		if !d.Signed {
			add(Finding{
				Rule: "DNS-003", Severity: SeverityMedium, Resource: resource,
				Title:    "DNSSEC not enabled",
				Detail:   "The parent zone holds no DS record, so answers for this domain can be forged on the path to a resolver or a certificate authority.",
				Controls: []string{"SOC2:CC6.6", "ISO27001:A.8.20"},
			})
		}
	}

	ours := map[string]bool{}
	for _, c := range e.OwnCertificates {
		ours[c.PubkeySHA256] = true
	}
	for _, key := range p.KnownPublicKeys {
		ours[strings.ToLower(key)] = true
	}
	seen := map[string]bool{}
	for _, i := range e.Issuances {
		if seen[i.ID] || i.Revoked || ours[i.PubkeySHA256] || now.Sub(i.NotBefore) > p.CTWindow {
			continue
		}
		seen[i.ID] = true
		names := append([]string(nil), i.DNSNames...)
		sort.Strings(names)
		add(Finding{
			Rule: "CT-001", Severity: SeverityCritical, Resource: "certificate/" + i.ID,
			Title: "Certificate issued that this project does not serve",
			Detail: fmt.Sprintf("%s issued a certificate for %s on %s with key %s, which no load balancer here holds. "+
				"If nobody here requested it, treat it as a hijack: lock the registrar, check NS and CAA, and ask %s to revoke it.",
				i.Issuer, strings.Join(names, ", "), i.NotBefore.Format(time.RFC3339), i.PubkeySHA256, i.Issuer),
			Controls: []string{"SOC2:CC7.2", "SOC2:CC6.7", "ISO27001:A.8.16"},
		})
	}
}

// caaIssuer is the issuer domain of an issue or issuewild value, without
// parameters: `letsencrypt.org; accounturi=...` is letsencrypt.org, and `;` is
// the empty string, which forbids issuance.
func caaIssuer(value string) string {
	issuer, _, _ := strings.Cut(value, ";")
	return strings.ToLower(strings.TrimSpace(issuer))
}
