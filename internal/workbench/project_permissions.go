package workbench

import (
	"errors"
	"fmt"
	"slices"

	"github.com/sylvinus/agent-vm/internal/cage"
)

type projectFolderMode string

const (
	projectFolderRead  projectFolderMode = "read"
	projectFolderWrite projectFolderMode = "write"
)

func (saved sharedProjects) folderMode(path string) projectFolderMode {
	for _, group := range saved.Groups {
		if mode, ok := group.FolderModes[path]; ok && slices.Contains(group.Folders, path) {
			return mode
		}
	}
	return projectFolderWrite
}

func (s *Server) projectFolderModes(saved sharedProjects) map[string]projectFolderMode {
	modes := make(map[string]projectFolderMode)
	for _, path := range s.projectPathsFromSaved(saved) {
		if !isClientMountPath(path) {
			modes[path] = saved.folderMode(path)
		}
	}
	return modes
}

func (s *Server) includePendingProjectFolderModes(modes map[string]projectFolderMode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingProjectGroup == nil {
		return
	}
	for folder, share := range s.pendingProjectGroup.shares {
		if _, committed := modes[folder]; committed || s.projectShares[folder] != share {
			continue
		}
		if share.probe != nil {
			modes[folder] = projectFolderRead
		} else {
			modes[folder] = projectFolderWrite
		}
	}
}

func requestedProjectFolderModes(requested map[string]projectFolderMode, selected map[string]string) (map[string]projectFolderMode, error) {
	modes := make(map[string]projectFolderMode, len(requested))
	for path, mode := range requested {
		folder, ok := selected[path]
		if !ok || (mode != projectFolderRead && mode != projectFolderWrite) {
			return nil, errors.New("请为已选中的文件夹指定只读或读写权限")
		}
		if old, exists := modes[folder]; exists && old != mode {
			return nil, fmt.Errorf("请为同一文件夹选择一致的权限：%s", folder)
		}
		modes[folder] = mode
	}
	return modes, nil
}

func (s *Server) checkProjectFolderMode(path string, mode projectFolderMode, grants map[string]projectFolderMode) error {
	if mode != projectFolderRead && mode != projectFolderWrite {
		return errors.New("无法识别项目文件夹权限，请在项目设置中重新选择")
	}
	physical, err := cage.StoragePhysicalPath(path)
	if err != nil {
		physical = path
	}
	if mode == projectFolderRead && s.macosGuest == nil && cage.StoragePathsOverlap(physical, s.environmentDir()) {
		return errors.New("此文件夹与 Cage 的读写传输目录重叠，请选择其他只读文件夹")
	}
	for other, otherMode := range grants {
		if other == path || otherMode == mode {
			continue
		}
		otherPhysical, err := cage.StoragePhysicalPath(other)
		if err != nil {
			otherPhysical = other
		}
		if cage.StoragePathsOverlap(physical, otherPhysical) || cage.StoragePathsOverlap(path, other) {
			return fmt.Errorf("共享权限冲突：%s 与 %s 重叠，请为重叠的文件夹选择相同权限", path, other)
		}
	}
	return nil
}
