package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Workspace is the stable default directory and preserves transcript identity.
// Paths remain the individual host grants; groups only associate those grants.
type projectGroup struct {
	Workspace   string                       `json:"workspace"`
	Name        string                       `json:"name"`
	Folders     []string                     `json:"folders"`
	FolderModes map[string]projectFolderMode `json:"folder_modes,omitempty"`
}

func (s *Server) readSharedProjects() (sharedProjects, error) {
	var saved sharedProjects
	data, err := os.ReadFile(filepath.Join(s.profile, "projects.json"))
	if os.IsNotExist(err) {
		return saved, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &saved)
	}
	return saved, err
}

func (s *Server) projectGroups() ([]projectGroup, error) {
	saved, err := s.readSharedProjects()
	if err != nil {
		return nil, err
	}
	paths := s.projectPathsFromSaved(saved)
	groups := []projectGroup{}
	covered := map[string]bool{}
	for _, group := range saved.Groups {
		if !slices.Contains(paths, group.Workspace) {
			continue
		}
		folders := []string{group.Workspace}
		for _, folder := range group.Folders {
			if slices.Contains(paths, folder) && !slices.Contains(folders, folder) {
				folders = append(folders, folder)
			}
		}
		group.Folders = folders
		groups = append(groups, group)
		for _, folder := range folders {
			covered[folder] = true
		}
	}
	for _, path := range paths {
		if !covered[path] {
			groups = append(groups, projectGroup{Workspace: path, Name: filepath.Base(path), Folders: []string{path}})
		}
	}
	return groups, nil
}

func (s *Server) projectFolders(workspace string) ([]string, error) {
	groups, err := s.projectGroups()
	if err != nil {
		return nil, err
	}
	for _, group := range groups {
		if slices.Contains(group.Folders, workspace) {
			return group.Folders, nil
		}
	}
	return []string{workspace}, nil
}

func (s *Server) projectAdditionalDirectories(workspace string) ([]string, error) {
	folders, err := s.projectFolders(workspace)
	if err != nil {
		return nil, err
	}
	var directories []string
	for _, folder := range folders {
		if folder == workspace {
			continue
		}
		if _, err := s.projectWorkspace(folder); err != nil {
			return nil, err
		}
		directory := s.cliWorkspace(folder)
		if s.windowsGuest != nil {
			binding, err := s.windowsProjectForHost(folder)
			if err != nil {
				return nil, err
			}
			directory = binding.Path
		}
		directories = append(directories, directory)
	}
	return directories, nil
}

// Saving and opening share the macOS approval flow. Approval never persists a
// partial group; the caller retries the complete request after GUI approval.
func (s *Server) projectConnectionError(w http.ResponseWriter, err error, folder string) *projectApprovalPrompt {
	if errors.Is(err, errMacOSProjectApproval) {
		if canonical, pathErr := validateWorkspace(folder, s.profile); pathErr == nil {
			folder = canonical
		}
		folders, _ := s.projectFolders(folder)
		s.mu.Lock()
		for _, member := range folders {
			if share := s.projectShares[member]; share != nil && share.approval != "" {
				folder = member
				break
			}
		}
		share := s.projectShares[folder]
		grant := ""
		if share != nil {
			grant = share.approval
		}
		s.mu.Unlock()
		if grant != "" {
			ctx, cancel := context.WithTimeout(s.operationContext(), 5*time.Second)
			_, openErr := s.guestControl(ctx, "/usr/bin/open -a /Applications/CageProjectAccess.app", "macos-project-approval")
			cancel()
			if openErr == nil {
				prompt := &projectApprovalPrompt{Workspace: folder, Approval: grant}
				jsonReply(w, prompt)
				return prompt
			}
			err = &projectShareError{Recovery: "project-approval", Message: "未能显示项目授权窗口，请重试"}
		}
	}
	var failure *projectShareError
	if errors.As(err, &failure) && failure.Recovery != "" {
		w.Header().Set("X-Cage-Recovery", failure.Recovery)
	}
	requestError(w, err)
	return nil
}

func (s *Server) saveProject(w http.ResponseWriter, r *http.Request) {
	var input projectGroup
	if !readJSON(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || utf8.RuneCountInString(input.Name) > 100 || strings.ContainsAny(input.Name, "\x00\r\n") || len(input.Folders) == 0 || len(input.Folders) > 32 {
		http.Error(w, "请填写项目名称，并添加 1–32 个文件夹", http.StatusBadRequest)
		return
	}
	if !s.op.TryLock() {
		http.Error(w, "另一个操作仍在进行，请稍候", http.StatusConflict)
		return
	}
	defer s.op.Unlock()
	s.mu.Lock()
	ready, closing := s.ready, s.closing
	s.mu.Unlock()
	if closing || !ready || s.busy() {
		http.Error(w, "环境尚未就绪或正在处理其他操作，请稍后重试", http.StatusConflict)
		return
	}
	saved, err := s.readSharedProjects()
	if err != nil {
		requestError(w, err)
		return
	}
	groups, err := s.projectGroups()
	if err != nil {
		requestError(w, err)
		return
	}
	// Validate every directory before opening any transport, including aliases.
	folders := []string{}
	aliases := make(map[string]string, len(input.Folders))
	for _, folder := range input.Folders {
		if isClientMountPath(folder) {
			http.Error(w, "临时客户端共享请单独管理", http.StatusBadRequest)
			return
		}
		path, err := validateWorkspace(folder, s.profile)
		if err != nil {
			requestError(w, err)
			return
		}
		if !slices.Contains(folders, path) {
			folders = append(folders, path)
		}
		aliases[folder] = path
	}
	requestedModes, err := requestedProjectFolderModes(input.FolderModes, aliases)
	if err != nil {
		requestError(w, err)
		return
	}
	var previous *projectGroup
	if input.Workspace != "" {
		for _, group := range groups {
			if group.Workspace == input.Workspace {
				value := group
				previous = &value
				break
			}
		}
		if previous == nil || !slices.Contains(folders, input.Workspace) {
			http.Error(w, "请保留项目的默认文件夹，以继续使用已有会话历史", http.StatusBadRequest)
			return
		}
	} else {
		input.Workspace = folders[0]
		if slices.ContainsFunc(saved.Groups, func(g projectGroup) bool { return g.Workspace == input.Workspace }) {
			http.Error(w, "默认文件夹已属于现有项目，请选择编辑该项目", http.StatusBadRequest)
			return
		}
	}
	for _, group := range groups {
		if group.Workspace == input.Workspace {
			continue
		}
		for _, folder := range folders {
			// An existing single-folder project can join this group. Named or
			// multi-folder groups must be edited explicitly to avoid stealing them.
			if slices.Contains(group.Folders, folder) && slices.ContainsFunc(saved.Groups, func(g projectGroup) bool { return g.Workspace == group.Workspace }) {
				http.Error(w, "文件夹已绑定到项目“"+group.Name+"”，请先从该项目解绑", http.StatusBadRequest)
				return
			}
		}
	}
	removed := []string{}
	if previous != nil {
		for _, folder := range previous.Folders {
			if !slices.Contains(folders, folder) {
				removed = append(removed, folder)
			}
		}
	}
	currentModes := s.projectFolderModes(saved)
	finalModes := maps.Clone(currentModes)
	for _, folder := range removed {
		delete(finalModes, folder)
	}
	input.FolderModes = nil
	var changed []string
	for _, folder := range folders {
		mode, specified := requestedModes[folder]
		if !specified {
			mode = saved.folderMode(folder)
		}
		if old, exists := currentModes[folder]; exists && old != mode {
			changed = append(changed, folder)
		}
		finalModes[folder] = mode
		if mode == projectFolderRead {
			if input.FolderModes == nil {
				input.FolderModes = make(map[string]projectFolderMode)
			}
			input.FolderModes[folder] = mode
		}
	}
	for _, folder := range folders {
		if err := s.checkProjectFolderMode(folder, finalModes[folder], finalModes); err != nil {
			requestError(w, err)
			return
		}
	}
	var affected []string
	if len(removed) > 0 {
		affected = previous.Folders
	}
	if len(changed) > 0 {
		affected = folders
		if len(removed) > 0 {
			affected = append(slices.Clone(folders), removed...)
		}
	}
	if err := s.checkProjectFoldersIdle(r.Context(), affected, true); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	prior := s.swapPendingProjectGroup(nil)
	if prior != nil && (prior.workspace != input.Workspace || !slices.Equal(prior.folders, folders) || !maps.Equal(prior.modes, input.FolderModes)) {
		s.cancelMacOSProjectApproval(prior.prompt.Workspace, prior.prompt.Approval)
		s.discardUncommittedProjectShares(prior.shares)
		prior = nil
	}
	committed := false
	var prompt *projectApprovalPrompt
	defer func() {
		if committed {
			return
		}
		if prior != nil && (prompt == nil || *prompt != prior.prompt) {
			s.cancelMacOSProjectApproval(prior.prompt.Workspace, prior.prompt.Approval)
		}
		staged := s.uncommittedProjectShares(folders, currentModes)
		if prompt == nil {
			s.discardUncommittedProjectShares(staged)
			return
		}
		s.swapPendingProjectGroup(&pendingProjectGroup{
			workspace: input.Workspace, folders: folders, modes: input.FolderModes, prompt: *prompt, shares: staged,
		})
	}()
	for _, folder := range folders {
		// Changed modes take effect only once saved. A replacement that overlaps
		// a revoked grant also waits until that grant has been detached.
		if slices.Contains(changed, folder) || s.checkProjectFolderMode(folder, finalModes[folder], currentModes) != nil {
			continue
		}
		if err := s.ensureProjectShareMode(folder, finalModes[folder]); err != nil {
			prompt = s.projectConnectionError(w, err, folder)
			return
		}
	}
	input.Folders = append([]string{input.Workspace}, slices.DeleteFunc(slices.Clone(folders), func(p string) bool { return p == input.Workspace })...)
	// Preserve the legacy current grant even when it was never in paths.
	paths := s.projectPathsFromSaved(saved)
	saved.Paths = slices.DeleteFunc(paths, func(p string) bool { return isClientMountPath(p) || slices.Contains(removed, p) })
	for _, folder := range folders {
		if !slices.Contains(saved.Paths, folder) {
			saved.Paths = append(saved.Paths, folder)
		}
	}
	index := slices.IndexFunc(saved.Groups, func(g projectGroup) bool { return g.Workspace == input.Workspace })
	if index >= 0 {
		saved.Groups[index] = input
	} else {
		saved.Groups = append(saved.Groups, input)
	}
	// A removed member may be the legacy current selection. Change it before
	// dropping its grant so projectPaths cannot resurrect it.
	if err := s.replaceRemovedWorkspace(removed, input.Workspace); err != nil {
		requestError(w, err)
		return
	}
	// Stop the old transports before publishing permission changes on disk.
	s.detachProjectFolders(changed)
	if err := writePrivate(filepath.Join(s.profile, "projects.json"), saved); err != nil {
		requestError(w, err)
		return
	}
	committed = true
	s.detachProjectFolders(removed)
	jsonReply(w, input)
}
