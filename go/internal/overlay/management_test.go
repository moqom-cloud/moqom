package overlay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeController is enough of the management API to exercise the client:
// certificate login, identities with a one-time token, enrolments, delete.
type fakeController struct {
	mu         sync.Mutex
	logins     int
	expireNext bool
	identities map[string]map[string]any
	nextID     int
	requests   []string
}

func newFake() *fakeController {
	return &fakeController{identities: map[string]map[string]any{}}
}

func (f *fakeController) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	reply := func(status int, data any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}
	if r.URL.Path == "/authenticate" {
		if r.URL.Query().Get("method") != "cert" {
			reply(http.StatusBadRequest, nil)
			return
		}
		f.logins++
		reply(http.StatusOK, map[string]string{"token": fmt.Sprintf("session-%d", f.logins)})
		return
	}
	if r.Header.Get("zt-session") != fmt.Sprintf("session-%d", f.logins) || f.expireNext {
		f.expireNext = false
		reply(http.StatusUnauthorized, nil)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/identities":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.nextID++
		id := fmt.Sprintf("id%d", f.nextID)
		body["id"] = id
		body["enrollment"] = map[string]any{"ott": map[string]any{
			"id": "e" + id, "jwt": "jwt-for-" + id, "expiresAt": "2026-10-06T15:00:00Z",
		}}
		body["authenticators"] = map[string]any{}
		f.identities[id] = body
		reply(http.StatusCreated, map[string]string{"id": id})
	case r.Method == http.MethodGet && r.URL.Path == "/identities":
		filter := r.URL.Query().Get("filter")
		var out []map[string]any
		for _, identity := range f.identities {
			if matches(identity, filter) {
				out = append(out, identity)
			}
		}
		reply(http.StatusOK, out)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/identities/"):
		identity, ok := f.identities[strings.TrimPrefix(r.URL.Path, "/identities/")]
		if !ok {
			reply(http.StatusNotFound, nil)
			return
		}
		reply(http.StatusOK, identity)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/identities/"):
		id := strings.TrimPrefix(r.URL.Path, "/identities/")
		if _, ok := f.identities[id]; !ok {
			reply(http.StatusNotFound, nil)
			return
		}
		delete(f.identities, id)
		reply(http.StatusOK, nil)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/enrollments/"):
		reply(http.StatusOK, nil)
	case r.Method == http.MethodPost && r.URL.Path == "/enrollments":
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		identity := f.identities[body["identityId"]]
		identity["enrollment"] = map[string]any{"ott": map[string]any{
			"id": "e2", "jwt": "reissued", "expiresAt": body["expiresAt"],
		}}
		reply(http.StatusCreated, nil)
	default:
		reply(http.StatusNotFound, nil)
	}
}

func matches(identity map[string]any, filter string) bool {
	if name, ok := strings.CutPrefix(filter, "name="); ok {
		return fmt.Sprintf("%q", identity["name"]) == name
	}
	if role, ok := strings.CutPrefix(filter, "anyOf(roleAttributes)="); ok {
		roles, _ := identity["roleAttributes"].([]any)
		for _, r := range roles {
			if fmt.Sprintf("%q", r) == role {
				return true
			}
		}
	}
	return false
}

func client(t *testing.T, fake *fakeController) *Client {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	return &Client{base: server.URL, http: server.Client()}
}

func TestCreateFindAndDelete(t *testing.T) {
	ctx := context.Background()
	fake := newFake()
	c := client(t, fake)

	enrolment, err := c.Create(ctx, NewIdentity{
		Name:           "moqom-dev-relay-us-west1-abcd",
		RoleAttributes: []string{"moqom-relay", "moqom-relay-managed"},
		Tags:           map[string]string{"instance_id": "123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if enrolment.JWT != "jwt-for-id1" || enrolment.ID != "id1" {
		t.Errorf("created %+v", enrolment)
	}
	if fake.identities["id1"]["isAdmin"] != false {
		t.Error("a minted identity must never be an overlay admin")
	}

	found, err := c.Find(ctx, "moqom-dev-relay-us-west1-abcd")
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != "id1" || found.Tags["instance_id"] != "123" || found.Enrolled {
		t.Errorf("found %+v", found)
	}
	managed, err := c.WithRole(ctx, "moqom-relay-managed")
	if err != nil || len(managed) != 1 {
		t.Errorf("WithRole found %v (%v)", managed, err)
	}

	if err := c.Delete(ctx, "id1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "id1"); err != nil {
		t.Errorf("deleting twice gave %v, want nil", err)
	}
	if _, err := c.Find(ctx, "moqom-dev-relay-us-west1-abcd"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete, Find gave %v", err)
	}
}

func TestATokenLifetimeReplacesTheDefault(t *testing.T) {
	fake := newFake()
	c := client(t, fake)
	until := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	enrolment, err := c.Create(context.Background(), NewIdentity{Name: "byoc-acme-1", TokenExpires: until})
	if err != nil {
		t.Fatal(err)
	}
	if enrolment.JWT != "reissued" || !enrolment.ExpiresAt.Equal(until) {
		t.Errorf("created %+v", enrolment)
	}
}

func TestALapsedSessionLogsInAgainOnce(t *testing.T) {
	ctx := context.Background()
	fake := newFake()
	c := client(t, fake)
	if _, err := c.Find(ctx, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	fake.expireNext = true
	if _, err := c.Find(ctx, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after a lapsed session: %v", err)
	}
	if fake.logins != 2 {
		t.Errorf("logged in %d times, want 2", fake.logins)
	}
}

func TestReadIdentityFileWantsInlinePEM(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := dir + "/" + name
		if err := writeFile(path, body); err != nil {
			t.Fatal(err)
		}
		return path
	}
	good := write("good.json", `{"ztAPI":"https://c:1280/edge/client/v1","id":{"cert":"pem:C","key":"pem:K","ca":"pem:A"}}`)
	file, err := ReadIdentityFile(good)
	if err != nil {
		t.Fatal(err)
	}
	if file.ID.Cert != "C" || file.ID.Key != "K" || file.ID.CA != "A" || file.ZtAPI == "" {
		t.Errorf("read %+v", file)
	}
	bad := write("bad.json", `{"id":{"cert":"file:///c","key":"pem:K","ca":"pem:A"}}`)
	if _, err := ReadIdentityFile(bad); err == nil {
		t.Error("a file reference was accepted")
	}
}
