package workbench

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sylvinus/agent-vm/internal/guestos"
)

// The isolated relay child uses the real profile/operation argv and JSON stdin.
func init() {
	if os.Getenv("CAGE_WINDOWS_CLI_RELAY_TEST") != "1" {
		return
	}
	if len(os.Args) != 4 || os.Args[1] != "--relay-profile" {
		os.Exit(2)
	}
	var input map[string]json.RawMessage
	if json.NewDecoder(os.Stdin).Decode(&input) != nil {
		os.Exit(2)
	}
	var profile, action string
	_ = json.Unmarshal(input["profile"], &profile)
	_ = json.Unmarshal(input["action"], &action)
	if os.Getenv("CAGE_WINDOWS_TEST_GUEST_WORKSPACE") != "" && (action == "open" || action == "terminal") {
		if string(input["workspace"]) != `"guest"` || input["project_id"] != nil || input["grant"] != nil {
			os.Exit(2)
		}
		if action == "open" && (os.Args[3] != "terminal" || string(input["shell"]) != `"powershell"`) ||
			action == "terminal" && (os.Args[3] != "cli" || string(input["kind"]) != `"claude"` || input["conversation"] == nil) {
			os.Exit(2)
		}
		fmt.Fprintln(os.Stdout, `{"ready":true}`)
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	if os.Args[3] != "cli" {
		os.Exit(2)
	}
	if len(profile) != 16 || input["command"] != nil {
		os.Exit(2)
	}
	if stage := os.Getenv("CAGE_WINDOWS_CLI_RELAY_PROCESS_FAILURE"); stage != "" {
		fmt.Fprintf(os.Stderr, "Windows guest channel unavailable (%s:IOException); private-token-must-not-appear\n", stage)
		os.Exit(1)
	}
	if os.Getenv("CAGE_WINDOWS_CLI_RELAY_WARNING") == "1" {
		fmt.Fprintln(os.Stderr, strings.Repeat("warning", 4096))
	}
	if os.Getenv("CAGE_WINDOWS_CLI_RELAY_FAILURE") == "1" {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"error": "requires_gui_login"})
		os.Exit(0)
	}
	switch action {
	case "account":
		if string(input["mode"]) != `"snapshot"` || string(input["account"]) != `"default"` {
			os.Exit(2)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"logged_in": false})
	case "channel":
		if workspace := os.Getenv("CAGE_WINDOWS_TEST_GUEST_WORKSPACE"); workspace != "" {
			var query map[string]any
			if json.Unmarshal(input["input"], &query) != nil || query["workspace"] != workspace || query["guest_workspace"] != true || query["project_id"] != nil || query["grant"] != nil {
				os.Exit(2)
			}
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"result": map[string]any{"ok": true}})
			break
		}
		if os.Getenv("CAGE_WINDOWS_CLI_RELAY_MOBILE") == "1" {
			var mode string
			_ = json.Unmarshal(input["mode"], &mode)
			var query map[string]any
			if json.Unmarshal(input["input"], &query) != nil || query["workspace"] != os.Getenv("CAGE_WINDOWS_CLI_RELAY_WORKSPACE") || query["project_id"] != strings.Repeat("a", 16) || query["grant"] != strings.Repeat("b", 64) {
				os.Exit(2)
			}
			if capture := os.Getenv("CAGE_WINDOWS_CLI_RELAY_CAPTURE"); capture != "" {
				file, err := os.OpenFile(capture, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
				if err != nil {
					os.Exit(2)
				}
				_ = json.NewEncoder(file).Encode(map[string]any{"mode": mode, "input": query})
				_ = file.Close()
			}
			if marker := os.Getenv("CAGE_WINDOWS_CLI_RELAY_BLOCK"); marker != "" {
				if os.WriteFile(marker, []byte("started"), 0600) != nil {
					os.Exit(2)
				}
				time.Sleep(time.Minute)
			}
			var result any = map[string]any{"ok": true}
			if mode == "view" {
				result = terminalConversation{CLIID: query["cli_id"].(string), Connection: query["connection"].(string), Config: query["config"].(string), Workspace: query["workspace"].(string), Running: true, CanSend: true, ChannelReady: true, Turns: []mobileTurn{}}
			}
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"result": result})
			break
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"result": []any{}})
	default:
		os.Exit(2)
	}
	os.Exit(0)
}
func windowsCLIRelayFixture(t *testing.T) *Server {
	t.Helper()
	s := testServer(t)
	binding := guestos.WindowsProfile{Version: 1, VMID: uuid.New(), GUIUserSID: "S-1-5-21-123-456-789-1001", GUIUser: "Guest", InstanceDirectory: filepath.Join(s.profile, "vm"), PairingFile: filepath.Join(s.profile, "pair.dpapi"), RelayExecutable: os.Args[0]}
	if err := guestos.BindWindowsProfile(s.profile, binding); err != nil {
		t.Fatal(err)
	}
	s.windowsGuest = &binding
	t.Setenv("CAGE_WINDOWS_CLI_RELAY_TEST", "1")
	return s
}
func TestWindowsCLINativeReportAndArrayTransport(t *testing.T) {
	s := windowsCLIRelayFixture(t)
	result, err := s.readWindowsClaudeAccount(context.Background(), "snapshot", "default", "")
	if err != nil || result.LoggedIn {
		t.Fatalf("native account report failed: %v", err)
	}
	var sessions []terminalConversation
	if err := s.windowsCLIRequest(context.Background(), map[string]any{"action": "channel", "mode": "discover", "input": map[string]any{"accounts": []string{"default"}, "workspaces": map[string]string{}}}, &sessions); err != nil || sessions == nil {
		t.Fatalf("native array response failed: %v", err)
	}
}
func TestWindowsCLIRejectsRevokedAndSiblingWorkspace(t *testing.T) {
	s := windowsCLIRelayFixture(t)
	path := `C:\Users\Guest\Cage\Projects\` + strings.Repeat("a", 16)
	share := &projectShare{windowsProject: &windowsProjectBinding{ID: strings.Repeat("a", 16), Grant: strings.Repeat("b", 64), Path: path}, done: make(chan struct{})}
	s.projectShares = map[string]*projectShare{"authorized": share}
	for _, bad := range []string{path + `-other`, path + `\..\private`} {
		if err := s.windowsCLIRequest(context.Background(), map[string]any{"action": "channel", "mode": "handoff", "input": map[string]any{"workspace": bad}}, new(any)); err == nil {
			t.Fatal("unmapped guest workspace admitted")
		}
	}
	close(share.done)
	if err := s.windowsCLIRequest(context.Background(), map[string]any{"action": "channel", "mode": "handoff", "input": map[string]any{"workspace": path}}, new(any)); err == nil {
		t.Fatal("revoked guest project admitted")
	}
	if !cliWorkspaceWithin(path+`\nested`, strings.ToLower(path), true) || cliWorkspaceWithin(path+`-other`, path, true) {
		t.Fatal("Windows workspace separator or case changed authorization")
	}
}

func TestWindowsGuestWorkspaceTerminalsAndHistory(t *testing.T) {
	s := windowsCLIRelayFixture(t)
	s.ready = true
	shutdown := func(controller *Server) {
		t.Cleanup(func() {
			controller.closeBackground()
			controller.terminals.close()
			controller.activities.Wait()
		})
	}
	shutdown(s)
	if _, err := s.createTerminal(terminalRequest{Workspace: guestWorkspaceID, Kind: "shell"}); err == nil || !strings.Contains(err.Error(), "更新") {
		t.Fatalf("old guest agent should offer a component update: %v", err)
	}
	const guestDir = `D:\Profiles\Guest\Cage\Workspace`
	s.windowsState.GuestWorkspace = guestDir
	t.Setenv("CAGE_WINDOWS_TEST_GUEST_WORKSPACE", guestDir)
	for _, kind := range []string{"shell", "claude"} {
		session, err := s.createTerminal(terminalRequest{Workspace: guestWorkspaceID, Kind: kind})
		if err != nil {
			t.Fatalf("%s terminal: %v", kind, err)
		}
		if session.summary()["workspace"] != guestWorkspaceID {
			t.Fatalf("%s session lost its workspace", kind)
		}
		id := session.summary()["cli_session_id"].(string)
		s.terminals.remove(session.id)
		if kind != "claude" {
			continue
		}
		record, err := s.mobileCLI.record(s.profile, id)
		if err != nil || record.Workspace != guestWorkspaceID || record.GuestWorkspace != guestDir {
			t.Fatalf("stored guest-only conversation: %v, %v", record, err)
		}
		input := map[string]any{}
		if err := s.windowsConversationInput(record, input); err != nil {
			t.Fatal(err)
		}
		var result struct{ OK bool }
		if err := s.windowsCLIRequest(t.Context(), map[string]any{"action": "channel", "mode": "view", "input": input}, &result); err != nil || !result.OK {
			t.Fatalf("guest conversation lookup: %+v, %v", result, err)
		}
		// A fresh controller has only the persisted record and the paired
		// guest's current status. It has no terminals, mounts or catalog.
		next := &Server{profile: s.profile, configPath: s.configPath, account: s.account, windowsGuest: s.windowsGuest, ready: true}
		next.windowsState.GuestWorkspace = guestDir
		shutdown(next)
		reopened, err := next.createTerminal(terminalRequest{Workspace: record.Workspace, Kind: "claude", sharedID: record.ID})
		if err != nil || reopened.summary()["cli_session_id"] != record.ID {
			t.Fatalf("could not reopen guest conversation: %v", err)
		}
	}
	if len(s.projectShares) != 0 {
		t.Fatal("guest-only workspace opened host shares")
	}
}

func TestWindowsCLIRPCFailureCannotSignAnAccountOut(t *testing.T) {
	s := windowsCLIRelayFixture(t)
	t.Setenv("CAGE_WINDOWS_CLI_RELAY_FAILURE", "1")
	if _, err := s.readWindowsClaudeAccount(context.Background(), "snapshot", "default", ""); err == nil || !strings.Contains(err.Error(), "Windows 桌面尚未登录或已锁定") {
		t.Fatalf("GUI/RPC failure lost its login explanation: %v", err)
	}
}

func TestWindowsCLIProcessFailureExplainsStageWithoutLeakingStderr(t *testing.T) {
	s := windowsCLIRelayFixture(t)
	for _, test := range []struct{ stage, message string }{
		{"profile", "无法读取 Windows 环境配置"},
		{"pairing", "安全配对校验未完成"},
		{"connect", "未能连接 Windows 会话服务"},
		{"transfer", "通信未完成"},
		{"unknown", "未能读取 Windows 会话服务的响应"},
	} {
		t.Run(test.stage, func(t *testing.T) {
			t.Setenv("CAGE_WINDOWS_CLI_RELAY_PROCESS_FAILURE", test.stage)
			err := s.windowsCLIRequest(context.Background(), map[string]any{"action": "channel", "mode": "discover"}, new(any))
			if err == nil || !strings.Contains(err.Error(), test.message) || strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), "官方账号") {
				t.Fatalf("relay failure was misclassified or exposed stderr: %v", err)
			}
		})
	}
}

func TestWindowsCLIRequestTimeoutExplainsDeadline(t *testing.T) {
	s := windowsCLIRelayFixture(t)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err := s.windowsCLIRequest(ctx, map[string]any{"action": "channel", "mode": "discover"}, new(any))
	if err == nil || !strings.Contains(err.Error(), "响应超时") {
		t.Fatalf("request timeout lost its cause: %v", err)
	}
}

func TestWindowsCLILongStderrDoesNotFailSuccessfulRequest(t *testing.T) {
	s := windowsCLIRelayFixture(t)
	t.Setenv("CAGE_WINDOWS_CLI_RELAY_WARNING", "1")
	if _, err := s.readWindowsClaudeAccount(context.Background(), "snapshot", "default", ""); err != nil {
		t.Fatalf("helper warning broke a successful response: %v", err)
	}
}
