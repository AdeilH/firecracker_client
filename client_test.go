package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCreateVMRequestShape(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(VM{ID: "vm-1", Name: "n", State: "running", SSHPort: 22})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	vm, err := c.CreateVM(CreateVMRequest{Name: "n", SSHKey: "ssh-ed25519 AAA"})
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q, want bearer token", gotAuth)
	}
	if vm.ID != "vm-1" || vm.State != "running" {
		t.Errorf("decoded VM = %+v", vm)
	}
	if gotBody["ssh_key"] != "ssh-ed25519 AAA" {
		t.Errorf("ssh_key = %v", gotBody["ssh_key"])
	}
	for _, absent := range []string{"cpus", "mem_mib"} {
		if _, ok := gotBody[absent]; ok {
			t.Errorf("body contains %q but zero values must be omitted: %v", absent, gotBody)
		}
	}

	// Non-zero cpus/mem must be sent.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["cpus"] != float64(2) || body["mem_mib"] != float64(512) {
			t.Errorf("body = %v, want cpus 2 and mem_mib 512", body)
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(VM{ID: "vm-2"})
	}))
	defer srv2.Close()
	if _, err := NewClient(srv2.URL, "tok").CreateVM(CreateVMRequest{Name: "n", CPUs: 2, MemMiB: 512}); err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
}

func TestClient401IsErrAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "bad").Capacity()
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
	if err.Error() != authNotice {
		t.Errorf("ErrAuth text = %q, want %q", err.Error(), authNotice)
	}
}

func TestClient409SurfacesServerMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"name already in use"}`))
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "tok").CreateVM(CreateVMRequest{Name: "dup"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v (%T), want *APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", apiErr.StatusCode)
	}
	if apiErr.Message != "name already in use" {
		t.Errorf("message = %q, want server-provided text", apiErr.Message)
	}
}

func TestClientUnreachableMessage(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "tok")
	_, err := c.Capacity()
	var unreachable *UnreachableError
	if !errors.As(err, &unreachable) {
		t.Fatalf("err = %v (%T), want *UnreachableError", err, err)
	}
	want := "host unreachable: is 127.0.0.1 online?"
	if err.Error() != want {
		t.Errorf("err = %q, want %q", err.Error(), want)
	}
}

func TestClientTimeoutGetsDistinctMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	c.HTTP.Timeout = 50 * time.Millisecond
	_, err := c.Capacity()
	var terr *TimeoutError
	if !errors.As(err, &terr) {
		t.Fatalf("err = %v (%T), want *TimeoutError on timeout", err, err)
	}
	if terr.Method != http.MethodGet || terr.Path != "/v1/capacity" {
		t.Errorf("TimeoutError = %+v, want GET /v1/capacity", terr)
	}
	if !strings.Contains(err.Error(), "timed out") || strings.Contains(err.Error(), "host unreachable") {
		t.Errorf("timeout must not be reported as unreachable: %q", err.Error())
	}
}

func TestClientStopStartDeletePaths(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewClient(srv.URL, "tok")

	mux.HandleFunc("/v1/vms/vm-1/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("stop method = %s, want POST", r.Method)
		}
		json.NewEncoder(w).Encode(VM{ID: "vm-1", State: "stopped"})
	})
	mux.HandleFunc("/v1/vms/vm-1/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("start method = %s, want POST", r.Method)
		}
		json.NewEncoder(w).Encode(VM{ID: "vm-1", State: "running"})
	})
	mux.HandleFunc("/v1/vms/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/vms/vm-1" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	if vm, err := c.StopVM("vm-1"); err != nil || vm.State != "stopped" {
		t.Errorf("StopVM = %+v, %v", vm, err)
	}
	if vm, err := c.StartVM("vm-1"); err != nil || vm.State != "running" {
		t.Errorf("StartVM = %+v, %v", vm, err)
	}
	if err := c.DeleteVM("vm-1"); err != nil {
		t.Errorf("DeleteVM: %v", err)
	}
}

func TestClientListVMsDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/vms" || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		w.Write([]byte(`{"vms":[{"id":"a"},{"id":"b"}]}`))
	}))
	defer srv.Close()

	vms, err := NewClient(srv.URL, "tok").ListVMs()
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(vms) != 2 || vms[0].ID != "a" || vms[1].ID != "b" {
		t.Errorf("vms = %+v", vms)
	}
}

func TestClientSendsNoAuthHeaderWithoutToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(204)
	}))
	defer srv.Close()

	if err := NewClient(srv.URL, "").DeleteVM("vm-1"); err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty", gotAuth)
	}
}

func TestVMOutputNeverHasPrivateKeyField(t *testing.T) {
	b, err := json.Marshal(vmOutput{ID: "vm-1", SSHCommand: "ssh ..."})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "ssh_private_key") {
		t.Errorf("vmOutput serialized a private-key field: %s", b)
	}
}
