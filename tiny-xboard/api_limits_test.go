package main

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// bigValidBody builds a valid compact push payload of roughly the given size.
func bigValidBody(entries int) string {
	var sb strings.Builder
	sb.WriteString("{")
	for i := 0; i < entries; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`"` + strconv.Itoa(i+1) + `":[1,1]`)
	}
	sb.WriteString("}")
	return sb.String()
}

// B#3: payload over the per-request limit -> 413, not 400, and no crash.
func TestPushBodyTooLargeReturns413(t *testing.T) {
	e := newAPIEnv(t, nil)
	body := bigValidBody(30000) // ~300 KB > 256 KB limit, valid JSON
	if len(body) <= maxPushBodySize {
		t.Fatalf("test payload (%d bytes) must exceed the %d-byte limit", len(body), maxPushBodySize)
	}
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/server/UniProxy/push"+query(e.token, "1", "vless"), bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

// B#3: a real-agent-sized payload (~30 KB, 1000 users) must always succeed.
func TestPushNormalAgentSizedBodyOK(t *testing.T) {
	s, _ := newTestStore(t)
	for i := 1; i <= 1000; i++ {
		mustAddUser(t, s, User{ID: i, UUID: "u" + strconv.Itoa(i)})
	}
	e := newAPIEnv(t, s)
	body := bigValidBody(1000)
	if len(body) >= maxPushBodySize {
		t.Fatalf("agent-sized payload must be far below the limit (got %d)", len(body))
	}
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/server/UniProxy/push"+query(e.token, "1", "vless"), bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// B#3: when the body-reader semaphore is saturated, a new push gets 503
// instead of queuing without bound.
func TestPushServerBusyReturns503(t *testing.T) {
	e := newAPIEnv(t, nil)
	for i := 0; i < cap(pushBodySem); i++ {
		pushBodySem <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(pushBodySem); i++ {
			<-pushBodySem
		}
	}()
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/server/UniProxy/push"+query(e.token, "1", "vless"), bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

// B#3: after the saturated readers drain, normal pushes succeed again.
func TestPushRecoversAfterBusy(t *testing.T) {
	for i := 0; i < cap(pushBodySem); i++ {
		pushBodySem <- struct{}{}
	}
	for i := 0; i < cap(pushBodySem); i++ {
		<-pushBodySem
	}
	e := newAPIEnv(t, nil)
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/server/UniProxy/push"+query(e.token, "1", "vless"), bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after readers drained", resp.StatusCode)
	}
}
