package gceidentity

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type staticKeys map[string]*rsa.PublicKey

func (s staticKeys) Key(_ context.Context, kid string) (*rsa.PublicKey, error) {
	if key, ok := s[kid]; ok {
		return key, nil
	}
	return nil, errors.New("unknown key")
}

var now = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func sign(t *testing.T, key *rsa.PrivateKey, header, body map[string]any) string {
	t.Helper()
	encode := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	unsigned := encode(header) + "." + encode(body)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func good() map[string]any {
	return map[string]any{
		"iss": "https://accounts.google.com", "aud": "moqom-relay-enroll",
		"email": "moqom-dev-relay@moqom-503700.iam.gserviceaccount.com", "email_verified": true,
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Hour).Unix(),
		"google": map[string]any{"compute_engine": map[string]any{
			"instance_id": "123", "instance_name": "moqom-dev-relay-us-west1-abcd",
			"project_id": "moqom-503700", "zone": "us-west1-b",
		}},
	}
}

func TestVerify(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	policy := Policy{
		Audience: "moqom-relay-enroll", Project: "moqom-503700", NamePrefix: "moqom-dev-relay-",
		ServiceAccounts: map[string]bool{"moqom-dev-relay@moqom-503700.iam.gserviceaccount.com": true},
		Keys:            staticKeys{"k1": &key.PublicKey},
		Now:             func() time.Time { return now },
	}
	header := map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"}

	instance, err := policy.Verify(context.Background(), sign(t, key, header, good()))
	if err != nil {
		t.Fatal(err)
	}
	if instance.Name != "moqom-dev-relay-us-west1-abcd" || instance.ID != "123" || instance.Zone != "us-west1-b" {
		t.Errorf("verified %+v", instance)
	}

	altered := func(change func(map[string]any)) map[string]any {
		body := good()
		change(body)
		return body
	}
	engine := func(body map[string]any) map[string]any {
		return body["google"].(map[string]any)["compute_engine"].(map[string]any)
	}
	refused := map[string]string{
		"another audience":   sign(t, key, header, altered(func(b map[string]any) { b["aud"] = "someone-else" })),
		"expired":            sign(t, key, header, altered(func(b map[string]any) { b["exp"] = now.Add(-2 * time.Minute).Unix() })),
		"another project":    sign(t, key, header, altered(func(b map[string]any) { engine(b)["project_id"] = "other" })),
		"another name":       sign(t, key, header, altered(func(b map[string]any) { engine(b)["instance_name"] = "dev-box" })),
		"another account":    sign(t, key, header, altered(func(b map[string]any) { b["email"] = "x@moqom-503700.iam.gserviceaccount.com" })),
		"no instance claims": sign(t, key, header, altered(func(b map[string]any) { delete(b, "google") })),
		"another signer":     sign(t, other, header, good()),
		"HS256":              sign(t, key, map[string]any{"alg": "HS256", "kid": "k1"}, good()),
		"garbage":            "not.a.token",
	}
	for name, token := range refused {
		if _, err := policy.Verify(context.Background(), token); !errors.Is(err, ErrNotTrusted) {
			t.Errorf("%s: got %v, want ErrNotTrusted", name, err)
		}
	}
	if _, err := (Policy{}).Verify(context.Background(), sign(t, key, header, good())); !errors.Is(err, ErrNotTrusted) {
		t.Error("an empty policy trusted a token")
	}
}
