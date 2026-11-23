package workbench

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestMacOSGuestWorkspaceOpensNativeShellWithoutAHostProject(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Keep the shipped setup and login shell commands. Only relocate the
	// fixture account's home; the other SSH commands use the usual Mac fixture.
	const guestDirectory = "/Users/cage/CageWorkspace"
	guest := newMacWorkspaceGuest(t, func(channel ssh.Channel, command string) (uint32, bool) {
		if !strings.Contains(command, guestDirectory) {
			return 0, false
		}
		command = strings.ReplaceAll(command, guestDirectory, `"$HOME/CageWorkspace"`)
		cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", command)
		cmd.Dir = home
		cmd.Env = []string{"HOME=" + home, "ZDOTDIR=" + home, "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
		if !strings.Contains(command, "/bin/zsh -l") {
			cmd.Stdout, cmd.Stderr = channel, channel.Stderr()
			if err := cmd.Run(); err != nil {
				t.Errorf("guest workspace setup: %v", err)
				return 1, true
			}
			return 0, true
		}
		tty, err := startTerminal(cmd, 100, 30)
		if err != nil {
			t.Errorf("start native zsh: %v", err)
			return 1, true
		}
		defer tty.Close()
		go func() { _, _ = io.Copy(tty, channel); _ = tty.Kill() }()
		_, _ = io.Copy(channel, tty)
		if err := tty.Wait(); err != nil {
			return 1, true
		}
		return 0, true
	})
	s := guest.s
	s.preferences.Workspace = ""

	response := nativeMaintenanceRequest(s, http.MethodPost, "/api/terminals", `{"workspace":"cage:guest-workspace","kind":"shell"}`)
	var opened struct{ ID, Workspace string }
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &opened) != nil || opened.Workspace != guestWorkspaceID {
		t.Fatalf("create terminal: %d %s", response.Code, response.Body.String())
	}
	session := s.terminals.get(opened.ID)
	sub := session.subscribe()
	defer sub.Close()
	if err := session.enqueueInput(t.Context(), terminalInputLine(`print -r -- "$PWD" > working-directory && print -r -- "${ZSH_VERSION:+native-zsh}:$(cat working-directory)" ; exit`)); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(home, "CageWorkspace")
	readTerminalUntil(t, sub, "native-zsh:"+directory)
	if data, err := os.ReadFile(filepath.Join(directory, "working-directory")); err != nil || string(data) != directory+"\n" {
		t.Fatalf("file written by login shell: %q, %v", data, err)
	}
	if paths, err := s.projectPaths(); err != nil || len(paths) != 0 || len(s.projectShares) != 0 {
		t.Fatalf("guest shell granted a host directory: %q, %v", paths, err)
	}
	input, _ := json.Marshal(terminalRequest{Workspace: guest.hostPath, Kind: "shell"})
	response = nativeMaintenanceRequest(s, http.MethodPost, "/api/terminals", string(input))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "请先通过打开项目添加该文件夹") {
		t.Fatalf("ungranted host path: %d %s", response.Code, response.Body.String())
	}
}
