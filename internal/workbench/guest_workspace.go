package workbench

import (
	"context"
	"errors"
	"fmt"
	"path"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/sylvinus/agent-vm/internal/guestprofile"
)

// This session identity is never a host path or a project grant.
const guestWorkspaceID = "cage:guest-workspace"

func (s *Server) guestWorkspaceAvailable() bool {
	return s.localVMStorage() && (s.windowsGuest == nil || !s.windowsGuest.Remote())
}

func (s *Server) guestWorkspacePath() string {
	if s.windowsGuest != nil {
		return s.windowsGuestWorkspacePath()
	}
	if s.macosGuest != nil {
		return path.Join("/Users", s.macosGuest.User, "CageWorkspace")
	}
	profile, err := guestprofile.Load(s.profile)
	if err != nil {
		return ""
	}
	return path.Join(profile.WorkHomePath(), "CageWorkspace")
}

func (s *Server) sessionWorkspace(workspace string) (string, error) {
	if workspace == guestWorkspaceID {
		if !s.guestWorkspaceAvailable() {
			return "", errors.New("当前环境不支持本机客机工作区")
		}
		return guestWorkspaceID, nil
	}
	return s.projectWorkspace(workspace)
}

// Prepare the directory as the ordinary guest user. The root must resolve
// to the expected directory on the home volume, including on later opens.
func guestWorkspaceSetup(directory string, macOS bool) string {
	device := "stat -c %d"
	if macOS {
		device = "stat -f %d"
	}
	return fmt.Sprintf(`set -eu
umask 077
dir=%s
test "$dir" = "$HOME/CageWorkspace"
test ! -L "$dir"
mkdir -p -- "$dir"
cd -P -- "$dir"
test "$PWD" = "$dir"
parent_device=$(%s "$HOME")
workspace_device=$(%s .)
test "$parent_device" = "$workspace_device"`, shellescape.Quote(directory), device, device)
}

func (s *Server) connectTerminalWorkspace(workspace string) (string, error) {
	if workspace != guestWorkspaceID {
		return s.connectProject(workspace, false)
	}
	if _, err := s.sessionWorkspace(workspace); err != nil {
		return "", err
	}
	directory := s.guestWorkspacePath()
	if directory == "" {
		return "", errors.New("无法确定客机工作区，请检查环境配置")
	}
	ctx, cancel := context.WithTimeout(s.operationContext(), 15*time.Second)
	defer cancel()
	if _, err := s.guest(ctx, guestWorkspaceSetup(directory, s.macosGuest != nil)); err != nil {
		return "", errors.New("无法打开客机用户主目录中的 CageWorkspace，请核对目录和客机连接")
	}
	return guestWorkspaceID, nil
}
