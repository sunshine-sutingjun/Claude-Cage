package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/mobilelink"
	"github.com/sylvinus/agent-vm/internal/relay"
)

// Build ./cmd/cage and pass its absolute path in CAGE_MOBILE_E2E_BINARY.
// This uses the actual desktop control gates, CLAUDE_CAGE_CONFIG isolation and
// server-entry command. An isolated profile suffices; no VM or model is used.
func TestIsolatedWorkbench(t *testing.T) {
	cage := os.Getenv("CAGE_MOBILE_E2E_BINARY")
	if cage == "" {
		t.Skip("set CAGE_MOBILE_E2E_BINARY to a newly built cage binary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"HOME", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, home)
	}
	profile := filepath.Join(dir, "profile")
	if err := os.Mkdir(profile, 0700); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(dir, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	project, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture := func(path string, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeNewPrivate(path, data); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(filepath.Join(profile, "projects.json"), map[string]any{"paths": []string{project}})
	// A stopped guest can serve persisted history and accept idempotent retries.
	// Starting the PTY for a brand-new conversation requires a running guest.
	const id = "b4fc408f-a017-4b2e-b95c-744e01af058e"
	history := filepath.Join(profile, "mobile-cli-sessions")
	if err := os.Mkdir(history, 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(filepath.Join(history, id+".json"), map[string]any{
		"id": id, "cli_id": "fdc145bd-d525-488f-be3d-1abeb79b728c", "workspace": project,
		"title": "Offline history", "revision": 1,
		"turns": []map[string]string{{"id": "54393beb-ac4b-4b44-9105-7da1d4d07eec", "status": "completed", "text": "Fixture reply"}},
	})
	config := filepath.Join(profile, "config.json")
	service := exec.CommandContext(ctx, cage, "serve")
	service.Env = append(os.Environ(), "CLAUDE_CAGE_CONFIG="+config)
	if err := service.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Process.Kill(); _ = service.Wait() }()
	awaitFile := func(file string) []byte {
		t.Helper()
		for ctx.Err() == nil {
			data, err := os.ReadFile(file)
			if err == nil && len(data) > 0 {
				return data
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("timed out waiting for isolated process output")
		return nil
	}
	var control struct {
		Port  json.Number
		Token string
	}
	if err := json.Unmarshal(awaitFile(filepath.Join(profile, "control.json")), &control); err != nil {
		t.Fatal("invalid isolated controller record")
	}
	desktopOrigin := "http://localhost:" + control.Port.String()
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, Transport: &http.Transport{Proxy: nil}, Timeout: 8 * time.Second}
	defer browser.CloseIdleConnections()
	var mobileOrigin, mobileToken string
	call := func(origin, method, route string, body any, want int) []byte {
		t.Helper()
		var data []byte
		if body != nil {
			var err error
			data, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, origin+route, bytes.NewReader(data))
		if err != nil {
			t.Fatal("invalid test URL")
		}
		req.Header.Set("Origin", origin)
		req.Header.Set("Content-Type", "application/json")
		if origin == mobileOrigin && mobileToken != "" {
			req.Header.Set("X-Cage-Mobile-Session", mobileToken)
		}
		res, err := browser.Do(req)
		if err != nil {
			t.Fatalf("request to %s failed", route)
		}
		defer res.Body.Close()
		if origin == mobileOrigin && route == "/link-session" {
			mobileToken = res.Header.Get("X-Cage-Mobile-Session")
		}
		if res.StatusCode != want {
			t.Fatalf("%s %s: status %d, want %d", method, route, res.StatusCode, want)
		}
		data, err = io.ReadAll(io.LimitReader(res.Body, 4<<20))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	var launch struct{ URL string }
	if json.Unmarshal(call(desktopOrigin, "POST", "/api/launch", map[string]string{"token": control.Token}, 200), &launch) != nil {
		t.Fatal("invalid local launch")
	}
	entry, err := url.Parse(launch.URL)
	if err != nil {
		t.Fatal("invalid local entry")
	}
	call(desktopOrigin, "POST", "/api/session", map[string]string{"token": entry.Fragment}, 200)

	stateHost, stateClient := filepath.Join(dir, "host"), filepath.Join(dir, "client")
	hostKey, err := createIdentity(stateHost)
	if err != nil {
		t.Fatal(err)
	}
	clientKey, err := createIdentity(stateClient)
	if err != nil {
		t.Fatal(err)
	}
	if err = trust(stateHost, clientKey.Pin); err != nil {
		t.Fatal(err)
	}
	if err = trust(stateClient, hostKey.Pin); err != nil {
		t.Fatal(err)
	}
	adminBytes := make([]byte, 32)
	_, _ = rand.Read(adminBytes)
	admin := hex.EncodeToString(adminBytes)
	r, err := relay.New(relay.Config{AdminToken: admin})
	if err != nil {
		t.Fatal(err)
	}
	var current atomic.Pointer[relay.Server]
	current.Store(r)
	rs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { current.Load().ServeHTTP(w, req) }))
	defer rs.Close()
	defer r.Close()
	tokenFile, inviteFile := filepath.Join(dir, "admin"), filepath.Join(dir, "invitation")
	if err := writeNewPrivate(tokenFile, []byte(admin)); err != nil {
		t.Fatal(err)
	}
	start := func(args ...string) func() {
		t.Helper()
		child, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- run(child, args, io.Discard, io.Discard) }()
		var once sync.Once
		closeService := func() {
			once.Do(func() {
				stop()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("link command did not exit")
				}
			})
		}
		t.Cleanup(closeService)
		return closeService
	}
	start("host", "--state", stateHost, "--relay", rs.URL, "--admin-token-file", tokenFile, "--invitation-out", inviteFile, "--cage-bin", cage, "--config", config)
	awaitFile(inviteFile)
	if os.Getenv("CAGE_MOBILE_E2E_DELAY_JOIN") == "1" {
		// A user may take longer than the 15-second TLS handshake deadline to
		// transport a private invitation. This wait belongs to relay admission.
		time.Sleep(16 * time.Second)
	}
	webDir := filepath.Join(dir, "web")
	if err := os.Mkdir(webDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(webDir, "mobile.html"), []byte("isolated mobile page"), 0600); err != nil {
		t.Fatal(err)
	}
	launchFile := filepath.Join(dir, "launch.html")
	start("client", "--state", stateClient, "--relay", rs.URL, "--invitation-file", inviteFile, "--web-dir", webDir, "--launch-file", launchFile)
	match := regexp.MustCompile(`url=(http://[^"]+)`).FindSubmatch(awaitFile(launchFile))
	if len(match) != 2 {
		t.Fatal("invalid private launch page")
	}
	mobileEntry, err := url.Parse(string(match[1]))
	if err != nil {
		t.Fatal("invalid local mobile origin")
	}
	mobileOrigin = mobileEntry.Scheme + "://" + mobileEntry.Host
	call(mobileOrigin, "POST", "/link-session", map[string]string{"token": mobileEntry.Fragment}, 204)
	call(mobileOrigin, "GET", "/api/mobile/cli/projects", nil, 423)
	pass := hex.EncodeToString(adminBytes[:16])
	call(desktopOrigin, "POST", "/api/account/create", map[string]string{"username": "isolated_mobile", "password": pass, "confirm_password": pass}, 200)
	var projects struct{ Projects []struct{ Path string } }
	if json.Unmarshal(call(mobileOrigin, "GET", "/api/mobile/cli/projects", nil, 200), &projects) != nil || len(projects.Projects) != 1 || projects.Projects[0].Path != project {
		t.Fatal("isolated workbench did not return its saved project")
	}
	before := awaitFile(filepath.Join(profile, "workbench.json"))
	call(mobileOrigin, "POST", "/api/mobile/cli/sessions", map[string]string{"id": "1d43e6d6-645e-467f-b27d-23a21a077eb5", "workspace": project}, 409)
	input := map[string]string{"id": id, "workspace": projects.Projects[0].Path}
	if view := call(mobileOrigin, "POST", "/api/mobile/cli/sessions", input, 200); !bytes.Contains(view, []byte("Fixture reply")) {
		t.Fatal("retried creation did not preserve the original conversation")
	}
	if !strings.Contains(string(call(mobileOrigin, "GET", "/api/mobile/cli/sessions", nil, 200)), id) {
		t.Fatal("saved session missing")
	}
	var snapshot struct{ Revision json.Number }
	if json.Unmarshal(call(mobileOrigin, "GET", "/api/mobile/cli/sessions/"+id, nil, 200), &snapshot) != nil || snapshot.Revision == "" {
		t.Fatal("invalid real session revision")
	}
	call(mobileOrigin, "GET", "/api/mobile/cli/sessions/"+id+"?after="+snapshot.Revision.String(), nil, 204)
	call(mobileOrigin, "POST", "/api/mobile/cli/sessions/"+id+"/metadata", map[string]string{"title": "Renamed offline"}, 200)
	if update := call(mobileOrigin, "GET", "/api/mobile/cli/sessions/"+id+"?after="+snapshot.Revision.String(), nil, 200); !bytes.Contains(update, []byte("Renamed offline")) {
		t.Fatal("changed history was missing from the revision update")
	}
	if after := awaitFile(filepath.Join(profile, "workbench.json")); !bytes.Equal(before, after) {
		t.Fatal("mobile session changed desktop preferences")
	}
	call(mobileOrigin, "POST", "/api/account/lock", map[string]string{}, 404)
	call(desktopOrigin, "POST", "/api/account/lock", map[string]string{}, 200)
	call(mobileOrigin, "GET", "/api/mobile/cli/sessions/"+id, nil, 423)
	call(desktopOrigin, "POST", "/api/account/unlock", map[string]string{"password": pass}, 200)
	savedFile := filepath.Join(dir, "saved-connection.json")
	stopHost := start("host", "--state", stateHost, "--relay", rs.URL, "--admin-token-file", tokenFile, "--connection-out", savedFile, "--cage-bin", cage, "--config", config)
	awaitFile(savedFile)
	var saved connectionFile
	if decodePrivateJSON(savedFile, &saved) != nil || !saved.valid() {
		t.Fatal("invalid exported saved connection")
	}
	previous := ""
	connectSaved := func() *mobilelink.Client {
		t.Helper()
		wait, cancel := context.WithTimeout(ctx, 6*time.Second)
		defer cancel()
		for wait.Err() == nil {
			room, err := resolveSaved(wait, savedFile, rs.URL, hostKey.Pin)
			if err == nil && room.ID != previous {
				c, err := mobilelink.ConnectClient(wait, rs.URL, room.ID, room.Token, clientKey, hostKey.Pin)
				if err != nil {
					t.Fatal(err)
				}
				v, err := c.Do(ctx, "GET", "/api/mobile/cli/sessions/"+id, nil)
				if err != nil || v.Status != 200 || !strings.Contains(string(v.Body), id) {
					c.Close()
					t.Fatal("saved connection lost original host session")
				}
				previous = room.ID
				return c
			}
			time.Sleep(30 * time.Millisecond)
		}
		t.Fatal("no new room from saved host")
		return nil
	}
	first := connectSaved()
	first.Close()
	second := connectSaved()
	newRelay, err := relay.New(relay.Config{AdminToken: admin})
	if err != nil {
		t.Fatal(err)
	}
	defer newRelay.Close()
	current.Store(newRelay)
	r.Close()
	second.Close()
	afterRelayRestart := connectSaved()
	stopHost()
	afterRelayRestart.Close()
	start("host", "--state", stateHost, "--serve-saved")
	afterHostRestart := connectSaved()
	if err := run(ctx, []string{"revoke", "--state", stateHost}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(3 * time.Second)
	for {
		_, err := afterHostRestart.Do(ctx, "GET", "/api/mobile/cli/sessions/"+id, nil)
		if err != nil {
			break
		}
		if time.Now().After(cutoff) {
			t.Fatal("revoked connection stayed usable")
		}
		time.Sleep(50 * time.Millisecond)
	}
	afterHostRestart.Close()
	if _, err := mobilelink.LookupRoom(ctx, rs.URL, saved.LookupToken); err == nil {
		t.Fatal("revoked token still resolved")
	}
	if err := trust(stateHost, clientKey.Pin); err != nil {
		t.Fatal(err)
	}
	newSaved, err := configureSavedHost(stateHost, filepath.Join(dir, "renewed.json"), hostKey.Pin, clientKey.Pin, hostSettings{Relay: rs.URL, AdminFile: tokenFile, CageBinary: cage, Config: config})
	if err != nil || newSaved.Connection.LookupToken == saved.LookupToken {
		t.Fatal("new authorization must rotate the lookup token")
	}
}
