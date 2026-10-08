package billing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Stripe is the small part of Stripe's API that billing uses.
//
// By hand rather than through stripe-go: five form-encoded POSTs and one HMAC,
// against a dependency that is regenerated weekly from Stripe's whole API. The
// cost is that a field Stripe renames reaches us as a test failure here instead
// of as a compile error there.
type Stripe struct {
	SecretKey string
	// BaseURL is https://api.stripe.com unless a test says otherwise.
	BaseURL string
	Client  *http.Client
}

// StripeError is an error Stripe returned.
type StripeError struct {
	Status  int
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *StripeError) Error() string {
	return fmt.Sprintf("stripe %d %s: %s", e.Status, e.Code, e.Message)
}

func (s *Stripe) post(ctx context.Context, path string, form url.Values, idempotencyKey string, into any) error {
	base := s.BaseURL
	if base == "" {
		base = "https://api.stripe.com"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	request.SetBasicAuth(s.SecretKey, "")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("stripe %s: %w", path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("stripe %s: reading the response: %w", path, err)
	}
	if response.StatusCode >= 300 {
		var envelope struct {
			Error StripeError `json:"error"`
		}
		_ = json.Unmarshal(body, &envelope)
		envelope.Error.Status = response.StatusCode
		return &envelope.Error
	}
	return json.Unmarshal(body, into)
}

// CreateCustomer creates the Stripe customer a tenant pays as.
func (s *Stripe) CreateCustomer(ctx context.Context, tenant, email, name string) (string, error) {
	form := url.Values{}
	form.Set("email", email)
	form.Set("name", name)
	form.Set("metadata[tenant]", tenant)
	var customer struct {
		ID string `json:"id"`
	}
	// Keyed on the tenant, so a retried provisioning job finds the customer the
	// first attempt made instead of making a second.
	err := s.post(ctx, "/v1/customers", form, "customer:"+tenant, &customer)
	return customer.ID, err
}

// Checkout is a hosted payment page for buying credit.
type Checkout struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// CreateCheckout starts a top-up. The card is saved for automatic top-ups, and
// the tenant is written into the payment intent's metadata, which is where the
// webhook reads it back from.
func (s *Stripe) CreateCheckout(ctx context.Context, tenant, customer string, amount Micros, successURL, cancelURL string) (Checkout, error) {
	form := url.Values{}
	form.Set("mode", "payment")
	form.Set("customer", customer)
	form.Set("client_reference_id", tenant)
	form.Set("success_url", successURL)
	form.Set("cancel_url", cancelURL)
	form.Set("line_items[0][quantity]", "1")
	form.Set("line_items[0][price_data][currency]", "usd")
	form.Set("line_items[0][price_data][unit_amount]", strconv.FormatInt(amount.Cents(), 10))
	form.Set("line_items[0][price_data][product_data][name]", "MOQOM Cloud credit")
	form.Set("payment_intent_data[setup_future_usage]", "off_session")
	form.Set("payment_intent_data[metadata][tenant]", tenant)
	form.Set("payment_intent_data[metadata][purpose]", "top_up")
	var checkout Checkout
	err := s.post(ctx, "/v1/checkout/sessions", form, "", &checkout)
	return checkout, err
}

// PaymentIntent is the subset of Stripe's payment intent billing reads.
type PaymentIntent struct {
	ID               string            `json:"id"`
	Status           string            `json:"status"`
	Amount           int64             `json:"amount"`
	AmountReceived   int64             `json:"amount_received"`
	Currency         string            `json:"currency"`
	Customer         string            `json:"customer"`
	PaymentMethod    string            `json:"payment_method"`
	SetupFutureUsage string            `json:"setup_future_usage"`
	Metadata         map[string]string `json:"metadata"`
	LastPaymentError *struct {
		Message string `json:"message"`
	} `json:"last_payment_error"`
}

// ChargeSavedCard runs an automatic top-up against the card on file.
func (s *Stripe) ChargeSavedCard(ctx context.Context, tenant, customer, paymentMethod string, amount Micros, idempotencyKey string) (PaymentIntent, error) {
	form := url.Values{}
	form.Set("amount", strconv.FormatInt(amount.Cents(), 10))
	form.Set("currency", "usd")
	form.Set("customer", customer)
	form.Set("payment_method", paymentMethod)
	form.Set("off_session", "true")
	form.Set("confirm", "true")
	form.Set("metadata[tenant]", tenant)
	form.Set("metadata[purpose]", "auto_top_up")
	var intent PaymentIntent
	err := s.post(ctx, "/v1/payment_intents", form, idempotencyKey, &intent)
	return intent, err
}

// CreatePortal opens Stripe's customer portal, where a tenant manages cards and
// downloads receipts without MOQOM building a billing UI.
func (s *Stripe) CreatePortal(ctx context.Context, customer, returnURL string) (string, error) {
	form := url.Values{}
	form.Set("customer", customer)
	form.Set("return_url", returnURL)
	var session struct {
		URL string `json:"url"`
	}
	err := s.post(ctx, "/v1/billing_portal/sessions", form, "", &session)
	return session.URL, err
}

// Event is a webhook delivery.
type Event struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

// Refund is the subset of Stripe's refund billing reads.
type Refund struct {
	ID            string `json:"id"`
	Amount        int64  `json:"amount"`
	Status        string `json:"status"`
	PaymentIntent string `json:"payment_intent"`
}

// ErrSignature is any webhook that cannot be shown to have come from Stripe.
var ErrSignature = errors.New("stripe webhook signature does not verify")

// WebhookTolerance is how old a signed delivery may be. Stripe's own default;
// it bounds how long a captured delivery can be replayed.
const WebhookTolerance = 5 * time.Minute

// VerifyWebhook checks the Stripe-Signature header and decodes the event.
func VerifyWebhook(payload []byte, header, secret string, now time.Time) (Event, error) {
	var timestamp string
	var signatures [][]byte
	for _, part := range strings.Split(header, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch name {
		case "t":
			timestamp = value
		case "v1":
			if decoded, err := hex.DecodeString(value); err == nil {
				signatures = append(signatures, decoded)
			}
		}
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || len(signatures) == 0 || secret == "" {
		return Event{}, ErrSignature
	}
	if age := now.Sub(time.Unix(seconds, 0)); age > WebhookTolerance || age < -WebhookTolerance {
		return Event{}, ErrSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(payload)
	expected := mac.Sum(nil)
	verified := false
	for _, signature := range signatures {
		if hmac.Equal(signature, expected) {
			verified = true
		}
	}
	if !verified {
		return Event{}, ErrSignature
	}
	var event Event
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(&event); err != nil {
		return Event{}, fmt.Errorf("decoding the event: %w", err)
	}
	return event, nil
}

// SignWebhook produces a Stripe-Signature header, for tests and for replaying a
// delivery by hand.
func SignWebhook(payload []byte, secret string, at time.Time) string {
	timestamp := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(payload)
	return "t=" + timestamp + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}
