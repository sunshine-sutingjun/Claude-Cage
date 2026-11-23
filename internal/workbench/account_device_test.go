package workbench

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeDeviceSessionBoundary(t *testing.T) {
	s := agentTestController(t)
	s.instanceID = randomToken()
	exchange := func(ticket string, native bool) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"token": ticket})
		r := httptest.NewRequest("POST", s.origin+"/api/session", bytes.NewReader(body))
		r.Header.Set("Origin", s.origin)
		r.Header.Set("Content-Type", "application/json")
		if native {
			r.Header.Set("X-Cage-Native-Device", "1")
		}
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, r)
		return w
	}
	deviceRequest := func(server *Server, path string, cookies []*http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", server.origin+"/api/account/"+path, strings.NewReader(`{"kind":"automatic"}`))
		r.Header.Set("Origin", server.origin)
		r.Header.Set("Content-Type", "application/json")
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		server.handler().ServeHTTP(w, r)
		return w
	}
	// A plain ticket stays a browser session even if a client asks for a native one.
	desktopTicket := func(native bool) string {
		entry, err := Desktop(s.configPath, DesktopOptions{NativeDevice: native})
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(entry.URL)
		if err != nil || u.Fragment == "" {
			t.Fatal("launcher did not issue a valid entry")
		}
		return u.Fragment
	}
	web := exchange(desktopTicket(false), true)
	if web.Code != http.StatusOK || len(web.Result().Cookies()) != 1 {
		t.Fatal("ordinary launch unexpectedly gained a device credential")
	}
	for _, action := range []string{"device-enroll", "device-revoke", "device-unlock"} {
		if deviceRequest(s, action, web.Result().Cookies()).Code != http.StatusForbidden {
			t.Fatalf("ordinary window could call %s", action)
		}
	}
	if len(s.account.record.Devices) != 0 {
		t.Fatal("rejected session changed device enrollment")
	}
	if desktop := exchange(desktopTicket(true), true); desktop.Code != http.StatusOK || len(desktop.Result().Cookies()) != 2 {
		t.Fatal("native desktop launcher could not bootstrap its Keychain session")
	}
	gateway, _ := gatewayFixture(t)
	if w := deviceRequest(gateway, "device-enroll", []*http.Cookie{{Name: gateway.cookieName(), Value: gateway.session}}); w.Code != http.StatusForbidden {
		t.Fatal("gateway did not check device authority before selecting an upstream")
	}
	// Use the actual SSH entry command result, which is private to the connector.
	entry, err := ServerEntry(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	legacy := exchange(entry.Ticket, false)
	if legacy.Code != http.StatusOK || len(legacy.Result().Cookies()) != 1 {
		t.Fatal("legacy connector should receive only its usual session")
	}
	entry, err = ServerEntry(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if wrong := exchange(entry.Ticket+"0", true); wrong.Code != http.StatusUnauthorized {
		t.Fatal("modified native ticket accepted")
	}
	native := exchange(entry.Ticket, true)
	if native.Code != http.StatusOK || len(native.Result().Cookies()) != 2 {
		t.Fatal("native bootstrap did not issue both session and device authority")
	}
	if replay := exchange(entry.Ticket, true); replay.Code != http.StatusUnauthorized {
		t.Fatal("native ticket was reusable")
	}
	w := deviceRequest(s, "device-enroll", native.Result().Cookies())
	var credential deviceCredential
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &credential) != nil || credential.Secret == "" || s.account.record.Devices["automatic"].ID != credential.ID {
		t.Fatal("native session could not enroll a device")
	}
	if w := deviceRequest(s, "device-revoke", native.Result().Cookies()); w.Code != http.StatusOK || len(s.account.record.Devices) != 0 {
		t.Fatal("native session could not revoke its device")
	}
}

func TestDeviceLoginRespectsPersistentManualLock(t *testing.T) {
	a := newTestAccount(t)
	automatic, err := a.enrollDevice("automatic")
	if err != nil {
		t.Fatal(err)
	}
	biometric, err := a.enrollDevice("biometric")
	if err != nil {
		t.Fatal(err)
	}
	original, _ := a.desktopKey()
	defer clear(original)
	data, _ := os.ReadFile(a.path)
	if bytes.Contains(data, []byte(automatic.Secret)) || bytes.Contains(data, original) {
		t.Fatal("device secret stored outside keychain")
	}
	a.lock() // App exit clears memory but allows the authorized automatic reopen.
	reopened, err := loadLocalAccount(filepath.Dir(a.path))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.lock()
	if err = reopened.unlockDevice("automatic", automatic); err != nil {
		t.Fatal(err)
	}
	if err = reopened.lockAccess(false); err != nil {
		t.Fatal(err)
	}
	locked, err := loadLocalAccount(filepath.Dir(a.path))
	if err != nil {
		t.Fatal(err)
	}
	defer locked.lock()
	if err = locked.unlockDevice("automatic", automatic); err == nil {
		t.Fatal("reopening bypassed manual lock")
	}
	if err = locked.unlockDevice("biometric", automatic); err == nil {
		t.Fatal("automatic token accepted as biometric proof")
	}
	if err = locked.unlockDevice("biometric", biometric); err != nil {
		t.Fatal(err)
	}
	after, _ := locked.desktopKey()
	defer clear(after)
	if !bytes.Equal(after, original) {
		t.Fatal("device unlock changed guest key")
	}
	if locked.view().RequireAuthentication {
		t.Fatal("authenticated unlock did not clear manual lock")
	}
	if err = locked.revokeDevice("biometric"); err != nil {
		t.Fatal(err)
	}
	locked.lock()
	if err = locked.unlockDevice("biometric", biometric); err == nil {
		t.Fatal("revoked credential accepted")
	}
	if err = locked.unlockDevice("automatic", automatic); err != nil {
		t.Fatal(err)
	}
	if err = locked.lockAccess(true); err != nil {
		t.Fatal(err)
	}
	if err = locked.unlockDevice("automatic", automatic); err == nil {
		t.Fatal("logout retained automatic authority")
	}
	if err = locked.unlock("initial-test-password"); err != nil {
		t.Fatal("password fallback lost", err)
	}
	if len(locked.view().Devices) != 0 {
		t.Fatal("logout retained device enrollment")
	}
}

func TestDeviceEnrollmentRequiresAccountAndAuthenticToken(t *testing.T) {
	a := newTestAccount(t)
	credential, err := a.enrollDevice("automatic")
	if err != nil {
		t.Fatal(err)
	}
	a.lock()
	if _, err = a.enrollDevice("biometric"); err == nil {
		t.Fatal("locked account issued a device token")
	}
	tampered := credential
	tampered.Secret = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if err = a.unlockDevice("automatic", tampered); err == nil {
		t.Fatal("forged device credential accepted")
	}
	if err = a.unlockDevice("automatic", credential); err != nil {
		t.Fatal(err)
	}
	replacement, err := a.enrollDevice("automatic")
	if err != nil {
		t.Fatal(err)
	}
	a.lock()
	if err = a.unlockDevice("automatic", credential); err == nil {
		t.Fatal("replaced device credential accepted")
	}
	if err = a.unlockDevice("automatic", replacement); err != nil {
		t.Fatal(err)
	}
}
