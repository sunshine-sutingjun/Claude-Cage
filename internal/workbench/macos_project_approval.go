package workbench

import (
	"net/http"
	"slices"
	"time"
)

type projectApprovalPrompt struct {
	Workspace string `json:"workspace"`
	Approval  string `json:"approval"`
}

type pendingProjectGroup struct {
	workspace string
	folders   []string
	modes     map[string]projectFolderMode
	prompt    projectApprovalPrompt
	shares    map[string]*projectShare
	timer     *time.Timer
}

const projectApprovalTimeout = 5 * time.Minute

func (s *Server) swapPendingProjectGroup(next *pendingProjectGroup) *pendingProjectGroup {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.pendingProjectGroup
	if previous != nil {
		previous.timer.Stop()
	}
	s.pendingProjectGroup = next
	if next != nil {
		next.timer = time.AfterFunc(projectApprovalTimeout, func() {
			s.op.Lock()
			defer s.op.Unlock()
			s.cancelMacOSProjectApproval(next.prompt.Workspace, next.prompt.Approval)
		})
	}
	return previous
}

func (s *Server) uncommittedProjectShares(folders []string, grants map[string]projectFolderMode) map[string]*projectShare {
	s.mu.Lock()
	defer s.mu.Unlock()
	shares := make(map[string]*projectShare)
	for _, folder := range folders {
		if _, committed := grants[folder]; !committed && s.projectShares[folder] != nil {
			shares[folder] = s.projectShares[folder]
		}
	}
	return shares
}

func (s *Server) discardUncommittedProjectShares(shares map[string]*projectShare) {
	if len(shares) == 0 {
		return
	}
	committed, _ := s.projectPaths()
	var discard []string
	s.mu.Lock()
	for folder, share := range shares {
		if s.projectShares[folder] == share && !slices.Contains(committed, folder) {
			discard = append(discard, folder)
		}
	}
	s.mu.Unlock()
	s.detachProjectFolders(discard)
}

// A pending project is tied to a single live, already user-selected SFTP
// stream. An abandoned picker must not leave that host directory accessible.
func (s *Server) beginMacOSProjectApproval(path string, share *projectShare) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if share.approval != "" {
		return
	}
	grant := randomToken()
	share.approval = grant
	share.approvalTimer = time.AfterFunc(projectApprovalTimeout, func() {
		s.op.Lock()
		defer s.op.Unlock()
		s.cancelMacOSProjectApproval(path, grant)
	})
}

func (s *Server) cancelMacOSProjectApproval(path, grant string) bool {
	s.mu.Lock()
	share := s.projectShares[path]
	matchedShare := share != nil && equalSecret(share.approval, grant)
	if matchedShare {
		delete(s.projectShares, path)
		share.approval = ""
	}
	pending := s.pendingProjectGroup
	matchedGroup := pending != nil && pending.prompt.Workspace == path && equalSecret(pending.prompt.Approval, grant)
	if matchedGroup {
		pending.timer.Stop()
		s.pendingProjectGroup = nil
	}
	s.mu.Unlock()
	if matchedShare {
		share.close()
	}
	if matchedGroup {
		s.discardUncommittedProjectShares(pending.shares)
	}
	return matchedShare || matchedGroup
}

func (s *Server) macOSProjectApproval(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Workspace string   `json:"workspace"`
		Grant     string   `json:"grant"`
		Action    string   `json:"action"`
		Window    uint32   `json:"window,omitempty"`
		PID       int32    `json:"pid,omitempty"`
		Nonce     string   `json:"nonce,omitempty"`
		X         *float64 `json:"x,omitempty"`
		Y         *float64 `json:"y,omitempty"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !s.op.TryLock() {
		http.Error(w, "正在处理其他客机操作，请重试", http.StatusConflict)
		return
	}
	defer s.op.Unlock()
	if req.Action == "cancel" {
		if !s.cancelMacOSProjectApproval(req.Workspace, req.Grant) {
			w.Header().Set("X-Cage-Recovery", "project-approval-ended")
			http.Error(w, "项目授权已结束", http.StatusConflict)
			return
		}
		jsonReply(w, map[string]bool{"ok": true})
		return
	}
	s.mu.Lock()
	share := s.projectShares[req.Workspace]
	active := s.macosGuest != nil && s.ready && !s.closing && share != nil && equalSecret(share.approval, req.Grant)
	s.mu.Unlock()
	if !active {
		w.Header().Set("X-Cage-Recovery", "project-approval-ended")
		http.Error(w, "项目授权请求已失效，请重新请求授权。", http.StatusConflict)
		return
	}
	select {
	case <-share.done:
		w.Header().Set("X-Cage-Recovery", "project-approval-ended")
		http.Error(w, "项目挂载已断开", http.StatusConflict)
		return
	default:
	}
	switch req.Action {
	case "frame", "click":
		input := macOSWindowRequest{Action: "project-" + req.Action, Project: macOSProjectPath(s.macosGuest.User, req.Workspace), Window: req.Window, PID: req.PID, Nonce: req.Nonce, X: req.X, Y: req.Y}
		s.macOSWindowRPC(w, r, input)
	default:
		http.Error(w, "未知的项目授权操作", http.StatusBadRequest)
	}
}
