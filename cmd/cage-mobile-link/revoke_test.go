package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sylvinus/agent-vm/internal/remotelink"
)

func TestRevokeClosesRunningClient(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "client")
	client, err := createIdentity(state)
	if err != nil {
		t.Fatal(err)
	}
	host, err := remotelink.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := trust(state, host.Pin); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	peerClosed := make(chan struct{})
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		conn, err := remotelink.Server(ctx, ws, host, client.Pin)
		if err != nil {
			return
		}
		defer conn.NetConn().Close()
		_, _ = io.Copy(io.Discard, conn)
		close(peerClosed)
	}))
	defer service.Close()
	invitation := filepath.Join(dir, "invitation")
	room, _ := json.Marshal(clientInvitation{strings.Repeat("A", 43), strings.Repeat("B", 42) + "A", time.Now().Add(time.Minute)})
	if err := writeNewPrivate(invitation, room); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mobile.html"), []byte("test page"), 0600); err != nil {
		t.Fatal(err)
	}
	launch := filepath.Join(dir, "launch.html")
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"client", "--state", state, "--relay", service.URL, "--invitation-file", invitation,
			"--web-dir", dir, "--launch-file", launch}, io.Discard, io.Discard)
	}()
	for {
		if _, err := os.Stat(launch); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("client ended before opening its bridge: %v", err)
		case <-ctx.Done():
			t.Fatal("client did not open its bridge")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := run(ctx, []string{"revoke", "--state", state}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, errRevoked) {
		t.Fatalf("client exit after revocation: %v", err)
	}
	select {
	case <-peerClosed:
	case <-ctx.Done():
		t.Fatal("authenticated connection remained open")
	}
}
