package server

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	"github.com/moqom-cloud/moqom/go/internal/apple"
	"github.com/moqom-cloud/moqom/go/internal/applesignin"
	"github.com/moqom-cloud/moqom/go/internal/control"
	"github.com/moqom-cloud/moqom/go/internal/token"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Apple sign-in hosted by MOQOM, for apps with no backend of their own.
//
// Three checks, each answering a different question. App Attest: is this a
// genuine copy of the tenant's app on a real Apple device? Sign in with Apple:
// who is the person? The device key, named in the identity token's nonce: which
// key will the token be bound to? All three must agree before anything is
// minted, and what is minted is bounded by the tenant's settings: rooms that
// already exist, the publish rights named, never moderation or ownership.
//
// A sign-in returns an enrollment: the person, the device key and the App
// Attest key, sealed for this control plane. Every renewal presents it with an
// App Attest assertion over the request, so each token is asked for by the same
// app on the same device. Apple's signatures are classical; a device's identity
// in a room is still its ML-DSA-87 key, and forging Apple's could at most enroll
// a new device, which joins as a new key every member sees.

const (
	// AppleSignInPath and AppleTokenPath are on the public listener.
	AppleSignInPath = "/v1/apple/sign-in"
	AppleTokenPath  = "/v1/apple/token"

	// AppleAssertionHeader carries the App Attest assertion on a renewal, made
	// over the SHA-256 of the request body.
	AppleAssertionHeader = "X-Moqom-Assertion"
)

// appleEnrollmentLifetime is how long a sign-in lasts before the person signs
// in again.
const appleEnrollmentLifetime = 30 * 24 * time.Hour

// howBigAnAppleSignInMayBe allows an identity token, an attestation and a key.
const howBigAnAppleSignInMayBe = 64 << 10

// howManyAppleMintsPerWindow bounds one person in one tenant.
const (
	howManyAppleMintsPerWindow = 30
	appleMintWindow            = 10 * time.Minute
)

// AppleSignInConfig turns the routes on.
type AppleSignInConfig struct {
	Settings applesignin.Store
	// Identity checks identity tokens; nil uses Apple's published keys.
	Identity *apple.IdentityVerifier
	// Roots is the App Attest root; nil is Apple's.
	Roots *x509.CertPool
}

type appleCore struct {
	settings applesignin.Store
	identity *apple.IdentityVerifier
	roots    *x509.CertPool
	limiter  *publicLimiter
}

func newAppleCore(config *AppleSignInConfig, now func() time.Time) *appleCore {
	if config == nil || config.Settings == nil {
		return nil
	}
	core := &appleCore{
		settings: config.Settings,
		identity: config.Identity,
		roots:    config.Roots,
		limiter:  &publicLimiter{per: howManyAppleMintsPerWindow, window: appleMintWindow, now: now},
	}
	if core.identity == nil {
		core.identity = apple.NewIdentityVerifier()
	}
	if core.roots == nil {
		core.roots = apple.AppAttestRoots()
	}
	return core
}

// AppleSignInRequest is a first sign-in.
type AppleSignInRequest struct {
	Tenant        string           `json:"tenant"`
	IdentityToken string           `json:"identityToken"`
	Attestation   string           `json:"attestation"`
	AttestKeyID   string           `json:"attestKeyId"`
	Key           *token.PublicKey `json:"key"`
	Room          string           `json:"room"`
}

// AppleTokenRequest is a renewal.
type AppleTokenRequest struct {
	Key  *token.PublicKey `json:"key"`
	Room string           `json:"room"`
}

// AppleSignInResponse is a token and the enrollment to renew it with.
type AppleSignInResponse struct {
	TokenResponse
	Enrollment          string    `json:"enrollment"`
	EnrollmentExpiresAt time.Time `json:"enrollmentExpiresAt"`
}

// appleEnrollment is what an enrollment seals.
type appleEnrollment struct {
	Version     int    `json:"v"`
	Tenant      string `json:"t"`
	Subject     string `json:"s"`
	Thumbprint  string `json:"j"`
	AttestKeyID string `json:"k"`
	AttestKey   []byte `json:"p"`
	AppID       string `json:"a"`
	Expires     int64  `json:"e"`
}

const appleEnrollmentVersion = 1

var errAppleUnavailable = errors.New("Apple sign-in is not available on this deployment")

func (s *Server) appleSignIn(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST a JSON body", http.StatusMethodNotAllowed)
		return
	}
	var asked AppleSignInRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, howBigAnAppleSignInMayBe)).Decode(&asked); err != nil {
		http.Error(w, "could not read the request", http.StatusBadRequest)
		return
	}
	response, code, err := s.core.appleSignIn(r.Context(), asked)
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	writeNoStore(w, response)
}

func (s *Server) appleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST a JSON body", http.StatusMethodNotAllowed)
		return
	}
	enrollment, ok := bearer(r)
	if !ok {
		http.Error(w, "an enrollment is required", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, howBigAPublicRequestMayBe))
	if err != nil {
		http.Error(w, "could not read the request", http.StatusBadRequest)
		return
	}
	var asked AppleTokenRequest
	if err := json.Unmarshal(body, &asked); err != nil {
		http.Error(w, "could not read the request", http.StatusBadRequest)
		return
	}
	assertion, err := base64.StdEncoding.DecodeString(r.Header.Get(AppleAssertionHeader))
	if err != nil || len(assertion) == 0 {
		http.Error(w, "an App Attest assertion over the body is required in "+AppleAssertionHeader, http.StatusUnauthorized)
		return
	}
	response, code, err := s.core.appleToken(r.Context(), enrollment, body, assertion, asked)
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	writeNoStore(w, response)
}

func writeNoStore(w http.ResponseWriter, response any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(response)
}

func (c *core) appleSignIn(ctx context.Context, asked AppleSignInRequest) (AppleSignInResponse, int, error) {
	if c.apple == nil || !c.sealer.enabled() {
		return AppleSignInResponse{}, http.StatusServiceUnavailable, errAppleUnavailable
	}
	now := c.now()
	settings, code, err := c.appleSettings(ctx, asked.Tenant, asked.Room)
	if err != nil {
		return AppleSignInResponse{}, code, err
	}
	device, err := boundDevice(asked.Key, "")
	if err != nil {
		return AppleSignInResponse{}, http.StatusBadRequest, err
	}
	identity, err := c.apple.identity.Verify(ctx, asked.IdentityToken, settings.BundleIDs, now)
	if err != nil {
		return AppleSignInResponse{}, http.StatusUnauthorized, err
	}
	if identity.Nonce != device {
		return AppleSignInResponse{}, http.StatusUnauthorized, errors.New(
			"the identity token's nonce is not the thumbprint of the key sent with it")
	}
	attestation, err := base64.StdEncoding.DecodeString(asked.Attestation)
	if err != nil {
		return AppleSignInResponse{}, http.StatusBadRequest, errors.New("attestation is not base64")
	}
	challenge := sha256.Sum256([]byte(asked.IdentityToken))
	attested, err := apple.VerifyAttestation(attestation, asked.AttestKeyID, challenge[:],
		settings.AppIDs(), c.apple.roots, now, settings.AllowDevelopment)
	if err != nil {
		return AppleSignInResponse{}, http.StatusUnauthorized, err
	}
	if attested.AppID != settings.TeamID+"."+identity.Audience {
		return AppleSignInResponse{}, http.StatusUnauthorized, errors.New(
			"the App Attest key and the identity token are for different apps")
	}
	if !c.apple.limiter.permit(asked.Tenant + "\x00" + identity.Subject) {
		return AppleSignInResponse{}, http.StatusTooManyRequests, errors.New("too many requests")
	}

	issued, code, err := c.appleMint(ctx, settings, identity.Subject, device, asked.Room)
	if err != nil {
		return AppleSignInResponse{}, code, err
	}
	point, err := attested.PublicKey.ECDH()
	if err != nil {
		return AppleSignInResponse{}, http.StatusInternalServerError, err
	}
	expires := now.Add(appleEnrollmentLifetime).Truncate(time.Second)
	enrollment, err := c.sealAppleEnrollment(appleEnrollment{
		Version:     appleEnrollmentVersion,
		Tenant:      asked.Tenant,
		Subject:     identity.Subject,
		Thumbprint:  device,
		AttestKeyID: asked.AttestKeyID,
		AttestKey:   point.Bytes(),
		AppID:       attested.AppID,
		Expires:     expires.Unix(),
	})
	if err != nil {
		return AppleSignInResponse{}, http.StatusInternalServerError, err
	}
	slog.InfoContext(ctx, "apple sign-in", "tenant", asked.Tenant, "username", issued.Username,
		"room", asked.Room, "development", attested.Development)
	return AppleSignInResponse{TokenResponse: issued, Enrollment: enrollment, EnrollmentExpiresAt: expires}, http.StatusOK, nil
}

func (c *core) appleToken(
	ctx context.Context,
	sealed string,
	body, assertion []byte,
	asked AppleTokenRequest,
) (TokenResponse, int, error) {
	if c.apple == nil || !c.sealer.enabled() {
		return TokenResponse{}, http.StatusServiceUnavailable, errAppleUnavailable
	}
	enrollment, err := c.openAppleEnrollment(sealed)
	if err != nil || enrollment.Version != appleEnrollmentVersion {
		return TokenResponse{}, http.StatusUnauthorized, errors.New("the enrollment is not valid: sign in again")
	}
	if c.now().Unix() >= enrollment.Expires {
		return TokenResponse{}, http.StatusUnauthorized, errors.New("the enrollment has expired: sign in again")
	}
	settings, code, err := c.appleSettings(ctx, enrollment.Tenant, asked.Room)
	if err != nil {
		return TokenResponse{}, code, err
	}
	if !slices.Contains(settings.AppIDs(), enrollment.AppID) {
		return TokenResponse{}, http.StatusUnauthorized, errors.New("the app is no longer allowed to sign in: sign in again")
	}
	device, err := boundDevice(asked.Key, "")
	if err != nil {
		return TokenResponse{}, http.StatusBadRequest, err
	}
	if device != enrollment.Thumbprint {
		return TokenResponse{}, http.StatusUnauthorized, errors.New("the key is not the one this enrollment was made for")
	}

	point, err := ecdh.P256().NewPublicKey(enrollment.AttestKey)
	if err != nil {
		return TokenResponse{}, http.StatusUnauthorized, errors.New("the enrollment is not valid: sign in again")
	}
	public, err := ecdsaFromECDH(point)
	if err != nil {
		return TokenResponse{}, http.StatusUnauthorized, errors.New("the enrollment is not valid: sign in again")
	}
	previous, err := c.apple.settings.Counter(ctx, enrollment.AttestKeyID)
	if err != nil {
		return TokenResponse{}, http.StatusServiceUnavailable, errors.New("could not check the assertion")
	}
	digest := sha256.Sum256(body)
	counter, err := apple.VerifyAssertion(assertion, digest[:], public, enrollment.AppID, previous)
	if err != nil {
		return TokenResponse{}, http.StatusUnauthorized, err
	}
	switch advanced, err := c.apple.settings.Advance(ctx, enrollment.AttestKeyID, counter); {
	case err != nil:
		return TokenResponse{}, http.StatusServiceUnavailable, errors.New("could not record the assertion")
	case !advanced:
		return TokenResponse{}, http.StatusUnauthorized, errors.New("the assertion was already used")
	}
	if !c.apple.limiter.permit(enrollment.Tenant + "\x00" + enrollment.Subject) {
		return TokenResponse{}, http.StatusTooManyRequests, errors.New("too many requests")
	}
	return c.appleMint(ctx, settings, enrollment.Subject, device, asked.Room)
}

// appleSettings reads a tenant's settings and checks the room against them.
func (c *core) appleSettings(ctx context.Context, tenant, room string) (applesignin.Settings, int, error) {
	if tenant == "" {
		return applesignin.Settings{}, http.StatusBadRequest, errors.New("tenant is required")
	}
	settings, err := c.apple.settings.Get(ctx, tenant)
	switch {
	case errors.Is(err, applesignin.ErrNotFound) || (err == nil && len(settings.BundleIDs) == 0):
		return applesignin.Settings{}, http.StatusNotFound, applesignin.ErrNotFound
	case err != nil:
		return applesignin.Settings{}, http.StatusServiceUnavailable, errors.New("could not read the tenant's settings")
	}
	if room == "" || !slices.Contains(settings.Rooms, room) {
		return applesignin.Settings{}, http.StatusForbidden, fmt.Errorf("room %q is not one Apple sign-in may mint for", room)
	}
	return settings, http.StatusOK, nil
}

// appleMint mints as the tenant's Apple sign-in, which may publish what the
// settings say and nothing else.
func (c *core) appleMint(
	ctx context.Context,
	settings applesignin.Settings,
	subject, device, room string,
) (TokenResponse, int, error) {
	principal := ServiceAccount{
		Name:   "apple-sign-in",
		Tenant: settings.Tenant,
		Grants: control.Grants{Publish: settings.Publish},
	}
	granted := control.Grants{Publish: settings.Publish}
	minted, err := tokensFor(c).MintClientToken(WithPrincipal(ctx, principal), &adminpb.MintClientTokenRequest{
		Tenant:           settings.Tenant,
		Username:         appleUsername(settings.Tenant, subject),
		DeviceThumbprint: device,
		Room:             room,
		Grants:           grantsToProto(granted),
		Lifetime:         durationpb.New(token.MaxLifetime),
	})
	if err != nil {
		return TokenResponse{}, statusToHTTP(err), err
	}
	return TokenResponse{
		Token:     minted.GetToken(),
		ExpiresAt: minted.GetExpiresAt().AsTime(),
		Tenant:    settings.Tenant,
		Room:      room,
		Username:  minted.GetIdentity().GetUsername(),
		KeyID:     minted.GetKeyId(),
		Device:    device,
		Publish:   publishNames(settings.Publish),
		Direct:    tokenDirect(minted.GetDirect()),
		Relays:    minted.GetRelays(),
	}, http.StatusOK, nil
}

// appleUsername names a person in one tenant without naming them anywhere
// else: the same Apple account is a different username in another tenant.
func appleUsername(tenant, subject string) string {
	digest := sha256.Sum256([]byte("moqom apple username\x00" + tenant + "\x00" + subject))
	return "apple-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:10]))
}

func publishNames(scope control.PublishScope) []string {
	var names []string
	for _, named := range []struct {
		scope control.PublishScope
		name  string
	}{
		{control.PublishAudio, "audio"},
		{control.PublishVideo, "video"},
		{control.PublishScreenShare, "screen_share"},
		{control.PublishData, "data"},
	} {
		if scope.Has(named.scope) {
			names = append(names, named.name)
		}
	}
	return names
}

// The enrollment is sealed with a key derived from each deployment seal key,
// so rotating those rotates this and an old enrollment opens until its key
// is removed.
func appleEnrollmentKey(master []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, master, nil, "moqom apple enrollment", 32)
}

const appleEnrollmentAAD = "moqom apple enrollment v1"

func (c *core) sealAppleEnrollment(enrollment appleEnrollment) (string, error) {
	plaintext, err := json.Marshal(enrollment)
	if err != nil {
		return "", err
	}
	key, err := appleEnrollmentKey(c.sealer.keys[0].Secret())
	if err != nil {
		return "", err
	}
	aead, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, plaintext, []byte(appleEnrollmentAAD))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (c *core) openAppleEnrollment(sealed string) (appleEnrollment, error) {
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		return appleEnrollment{}, err
	}
	for _, master := range c.sealer.keys {
		key, err := appleEnrollmentKey(master.Secret())
		if err != nil {
			return appleEnrollment{}, err
		}
		aead, err := newGCM(key)
		if err != nil {
			return appleEnrollment{}, err
		}
		if len(raw) < aead.NonceSize() {
			return appleEnrollment{}, errors.New("short enrollment")
		}
		plaintext, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte(appleEnrollmentAAD))
		if err != nil {
			continue
		}
		var enrollment appleEnrollment
		if err := json.Unmarshal(plaintext, &enrollment); err != nil {
			return appleEnrollment{}, err
		}
		return enrollment, nil
	}
	return appleEnrollment{}, errors.New("no key opens the enrollment")
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// ecdsaFromECDH turns an uncompressed P-256 point back into the key App
// Attest assertions are verified with.
func ecdsaFromECDH(point *ecdh.PublicKey) (*ecdsa.PublicKey, error) {
	return ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point.Bytes())
}

var (
	appleTeamID   = regexp.MustCompile(`^[A-Z0-9]{10}$`)
	appleBundleID = regexp.MustCompile(`^[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$`)
)

const howManyAppleBundlesOrRooms = 64

// SetAppleSignIn is for an owner key only: it decides who strangers on the
// internet become in the tenant's rooms.
func (s *tokensService) SetAppleSignIn(
	ctx context.Context,
	request *adminpb.SetAppleSignInRequest,
) (*adminpb.AppleSignIn, error) {
	if s.core.apple == nil {
		return nil, status.Error(codes.Unimplemented, errAppleUnavailable.Error())
	}
	account, err := requireTenant(ctx, request.GetTenant())
	if err != nil {
		return nil, err
	}
	if !account.Grants.IsOwner {
		return nil, status.Errorf(codes.PermissionDenied, "account %s is not an owner key", account.Name)
	}
	settings := applesignin.Settings{
		Tenant:           request.GetTenant(),
		TeamID:           request.GetTeamId(),
		BundleIDs:        slices.Compact(slices.Sorted(slices.Values(request.GetBundleIds()))),
		Rooms:            slices.Compact(slices.Sorted(slices.Values(request.GetRooms()))),
		AllowDevelopment: request.GetAllowDevelopment(),
		UpdatedAt:        s.core.now().UTC(),
	}
	if len(settings.BundleIDs) > 0 {
		if !appleTeamID.MatchString(settings.TeamID) {
			return nil, status.Error(codes.InvalidArgument, "team_id is the ten-character Apple team ID")
		}
		if len(settings.BundleIDs) > howManyAppleBundlesOrRooms || len(settings.Rooms) > howManyAppleBundlesOrRooms {
			return nil, status.Errorf(codes.InvalidArgument, "at most %d bundle IDs and %d rooms",
				howManyAppleBundlesOrRooms, howManyAppleBundlesOrRooms)
		}
		for _, bundle := range settings.BundleIDs {
			if !appleBundleID.MatchString(bundle) {
				return nil, status.Errorf(codes.InvalidArgument, "%q is not a bundle ID", bundle)
			}
		}
		if len(settings.Rooms) == 0 {
			return nil, status.Error(codes.InvalidArgument, "name at least one room")
		}
		for _, room := range settings.Rooms {
			if _, err := s.core.store.GetRoom(ctx, settings.Tenant, room); err != nil {
				return nil, statusFor(err)
			}
		}
	}
	grants, err := grantsFromProto(&adminpb.Grants{Publish: request.GetPublish()}, adminpb.CapabilityPreset_CAPABILITY_PRESET_UNSPECIFIED)
	if err != nil {
		return nil, statusFor(err)
	}
	settings.Publish = grants.Publish
	if err := s.core.apple.settings.Put(ctx, settings); err != nil {
		return nil, status.Error(codes.Unavailable, "could not save the settings")
	}
	slog.InfoContext(ctx, "apple sign-in settings", "tenant", settings.Tenant, "account", account.Name,
		"bundles", len(settings.BundleIDs), "rooms", len(settings.Rooms))
	return appleSignInToProto(settings), nil
}

func (s *tokensService) GetAppleSignIn(
	ctx context.Context,
	request *adminpb.GetAppleSignInRequest,
) (*adminpb.AppleSignIn, error) {
	if s.core.apple == nil {
		return nil, status.Error(codes.Unimplemented, errAppleUnavailable.Error())
	}
	if _, err := requireTenant(ctx, request.GetTenant()); err != nil {
		return nil, err
	}
	settings, err := s.core.apple.settings.Get(ctx, request.GetTenant())
	switch {
	case errors.Is(err, applesignin.ErrNotFound):
		settings = applesignin.Settings{Tenant: request.GetTenant()}
	case err != nil:
		return nil, status.Error(codes.Unavailable, "could not read the settings")
	}
	return appleSignInToProto(settings), nil
}

func appleSignInToProto(settings applesignin.Settings) *adminpb.AppleSignIn {
	wire := &adminpb.AppleSignIn{
		Tenant:           settings.Tenant,
		TeamId:           settings.TeamID,
		BundleIds:        settings.BundleIDs,
		Rooms:            settings.Rooms,
		Publish:          grantsToProto(control.Grants{Publish: settings.Publish}).GetPublish(),
		AllowDevelopment: settings.AllowDevelopment,
		SignInPath:       AppleSignInPath,
		TokenPath:        AppleTokenPath,
	}
	if !settings.UpdatedAt.IsZero() {
		wire.UpdatedAt = timestamppb.New(settings.UpdatedAt)
	}
	return wire
}
