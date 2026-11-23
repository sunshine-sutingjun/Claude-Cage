//go:build !windows

package workbench

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/guestos"
)

func TestGuestWorkspaceTerminalKeepsFilesWithoutHostGrant(t *testing.T) {
	s := testServer(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.ready = true
	mobileTestSSHRewrite(t, s, home, []string{"HOME=" + home, "PATH=/usr/bin:/bin"}, func(command string) string {
		// Run the shipped SSH commands against a temporary guest home on this OS.
		command = strings.ReplaceAll(command, "/home/cage/CageWorkspace", filepath.ToSlash(home)+"/CageWorkspace")
		if runtime.GOOS == "darwin" {
			command = strings.ReplaceAll(command, "stat -c %d", "stat -f %d")
		}
		return command
	})
	t.Cleanup(s.terminals.close)
	open := func(s *Server, command, want string) {
		t.Helper()
		response := request(s, http.MethodPost, "/api/terminals", s.origin, `{"workspace":"cage:guest-workspace","kind":"shell"}`, true)
		var view struct{ ID, Workspace string }
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &view) != nil || view.Workspace != guestWorkspaceID {
			t.Fatalf("open guest terminal: %d %s", response.Code, response.Body.String())
		}
		session := s.terminals.get(view.ID)
		sub := session.subscribe()
		defer sub.Close()
		if err := session.enqueueInput(t.Context(), terminalInputLine(command)); err != nil {
			t.Fatal(err)
		}
		readTerminalUntil(t, sub, want)
		s.terminals.remove(session.id)
	}
	open(s, "printf retained > example && printf '%s%s\\n' first- done", "first-done")
	reopened := testServer(t)
	reopened.profile, reopened.configPath, reopened.ready = s.profile, s.configPath, true
	t.Cleanup(reopened.terminals.close)
	open(reopened, "printf 'from-guest-%s\\n' \"$(cat example)\"", "from-guest-retained")
	if data, err := os.ReadFile(filepath.Join(home, "CageWorkspace", "example")); err != nil || string(data) != "retained" {
		t.Fatalf("guest file = %q, %v", data, err)
	}
	paths, err := reopened.projectPaths()
	if err != nil || len(paths) != 0 || reopened.workspace() != "" || len(reopened.projectShares) != 0 || len(s.projectShares) != 0 {
		t.Fatalf("guest-only work granted host paths: %v, %v", paths, err)
	}
	if _, err := s.projectWorkspace(guestWorkspaceID); err == nil {
		t.Fatal("guest-only session identity accepted as a host project")
	}
}

func TestGuestWorkspaceClaudeHistoryAfterReopen(t *testing.T) {
	for _, system := range []string{"linux", "macos"} {
		t.Run(system, func(t *testing.T) {
			s := testServer(t)
			if system == "macos" {
				s.macosGuest = &guestos.MacOSProfile{User: "testuser"}
			}
			script, first, err := s.prepareTerminalConversation(guestWorkspaceID, claudeSelection{}, "")
			if err != nil {
				t.Fatal(err)
			}
			wantPath := "/home/cage/CageWorkspace"
			if system == "macos" {
				wantPath = "/Users/testuser/CageWorkspace"
			}
			if !strings.Contains(script, wantPath) || first.GuestWorkspace != wantPath || strings.Contains(script, "cage-macos-terminal ") {
				t.Fatalf("incorrect guest launch directory: %s, %s", first.GuestWorkspace, script)
			}
			reopened := testServer(t)
			reopened.profile, reopened.macosGuest = s.profile, s.macosGuest
			w := nativeMaintenanceRequest(reopened, http.MethodGet, "/api/cli/sessions", "")
			var listed struct{ Sessions []mobileSummary }
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listed) != nil || len(listed.Sessions) != 1 || listed.Sessions[0].ID != first.ID {
				t.Fatalf("lost guest session on reopen: %d %s", w.Code, w.Body.String())
			}
			_, resumed, err := reopened.prepareTerminalConversation(guestWorkspaceID, claudeSelection{}, first.ID)
			if err != nil || resumed.CLIID != first.CLIID || resumed.GuestWorkspace != wantPath {
				t.Fatalf("resumed another conversation or directory: %#v, %v", resumed, err)
			}
		})
	}
}
