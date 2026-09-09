package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeAPI struct {
	mu                                      sync.Mutex
	running                                 map[string]any
	after                                   map[string]any
	batches                                 [][]Command
	reads, activeReads, saves               int
	failSave, failConfigure, commitThenFail bool
	server                                  *httptest.Server
}

func newFakeAPI(t *testing.T, running, after map[string]any) *fakeAPI {
	t.Helper()
	f := &fakeAPI{running: running, after: after}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if req.Method != http.MethodPost || req.ParseForm() != nil || req.Form.Get("key") != "test-api-key" {
			t.Error("invalid authentication or request method")
			w.WriteHeader(400)
			return
		}
		var data any
		switch req.URL.Path {
		case "/retrieve":
			var payload map[string]any
			if err := json.Unmarshal([]byte(req.Form.Get("data")), &payload); err != nil {
				t.Error(err)
			}
			if payload["op"] != "showConfig" || payload["configFormat"] != "json_ast" || !reflect.DeepEqual(payload["path"], []any{}) {
				t.Errorf("unexpected read: %v", payload)
			}
			f.reads++
			data = fixtureAST("", f.running)
		case "/show":
			var payload Command
			if err := json.Unmarshal([]byte(req.Form.Get("data")), &payload); err != nil {
				t.Error(err)
			}
			if payload.Op != "show" || !reflect.DeepEqual(payload.Path, []string{"configuration", "commands"}) {
				t.Errorf("unexpected active export: %v", payload)
			}
			f.activeReads++
			data = fixtureCommands(nil, f.running)
		case "/configure":
			var operations []Command
			if err := json.Unmarshal([]byte(req.Form.Get("data")), &operations); err != nil {
				t.Error("batch must be a JSON array")
			}
			if len(operations) == 0 {
				t.Error("empty configure batch")
			}
			f.batches = append(f.batches, operations)
			if f.after != nil && (!f.failConfigure || f.commitThenFail) {
				f.running = f.after
			}
			if f.failConfigure {
				w.WriteHeader(500)
				_, _ = w.Write([]byte("test-api-key and private-router-secret"))
				return
			}
		case "/config-file":
			var payload map[string]string
			if err := json.Unmarshal([]byte(req.Form.Get("data")), &payload); err != nil {
				t.Error(err)
			}
			if payload["op"] != "save" {
				t.Errorf("unexpected config-file operation: %v", payload)
			}
			f.saves++
			if f.failSave {
				w.WriteHeader(500)
				return
			}
		default:
			t.Error("unexpected API path")
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data, "error": nil})
	}))
	t.Cleanup(f.server.Close)
	return f
}

func TestClientBatchAndSave(t *testing.T) {
	f := newFakeAPI(t, map[string]any{}, nil)
	c, err := newClient(f.server.URL, "test-api-key", false, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	if _, err := c.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	ops := commandsFixture(t, []string{"set interfaces dummy dum99 description 'hello world'", "set interfaces dummy dum99 mtu 1450"})
	if err := c.Configure(context.Background(), ops); err != nil {
		t.Fatal(err)
	}
	if err := c.Configure(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.reads != 1 || f.activeReads != 1 || len(f.batches) != 1 || len(f.batches[0]) != 2 || f.saves != 1 {
		t.Fatalf("unexpected round trips: %+v", f)
	}
}

func TestClientFailuresNeverEchoSecrets(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"HTTP failure", "test-api-key private-secret", 500},
		{"API failure", `{"success":false,"error":"test-api-key private-secret"}`, 200},
		{"missing success", `{"data":null}`, 200},
		{"malformed", `test-api-key private-secret`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer s.Close()
			c, _ := newClient(s.URL, "test-api-key", false, time.Second)
			defer c.close()
			err := c.Save(context.Background())
			if err == nil || strings.Contains(err.Error(), "test-api-key") || strings.Contains(err.Error(), "private-secret") {
				t.Fatalf("unsafe error: %v", err)
			}
		})
	}
}

func TestClientTLSRedirectAndCancellation(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"data":null,"error":null}`))
	}))
	defer s.Close()
	for _, insecure := range []bool{false, true} {
		c, _ := newClient(s.URL, "test-api-key", insecure, time.Second)
		err := c.Save(context.Background())
		c.close()
		if (err == nil) != insecure {
			t.Fatalf("insecure=%v err=%v", insecure, err)
		}
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { t.Error("followed a credential redirect") }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	c, _ := newClient(redirect.URL, "test-api-key", false, time.Second)
	defer c.close()
	if err := c.Save(context.Background()); err == nil {
		t.Fatal("accepted redirect")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Save(ctx); err == nil {
		t.Fatal("ignored cancellation")
	}
	for _, endpoint := range []string{"router", "ftp://router", "https://user:secret@router", "https://router?key=secret", "https://router#secret"} {
		if _, err := normalizeEndpoint(endpoint); err == nil {
			t.Errorf("accepted %s", endpoint)
		}
	}
}

func TestRouterLock(t *testing.T) {
	ctx := context.Background()
	unlock, err := lockRouter(ctx, "test-lock")
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := lockRouter(cancelled, "test-lock"); err == nil {
		t.Fatal("same router lock did not block")
	}
	other, err := lockRouter(ctx, "other-router")
	if err != nil {
		t.Fatal(err)
	}
	other()
	unlock()
	unlock, err = lockRouter(ctx, "test-lock")
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}
