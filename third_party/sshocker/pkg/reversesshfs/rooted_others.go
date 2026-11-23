//go:build !linux && !darwin && !windows

package reversesshfs

import (
	"errors"
	"io"

	"github.com/pkg/sftp"
)

type rootedHandlers struct{}

func newRootedServer(io.ReadWriteCloser, string, bool, []string) (*sftp.RequestServer, *rootedHandlers, error) {
	return nil, nil, errors.New("the rooted builtin sftp server is supported only on Linux, macOS, and Windows")
}

func newRootedServerAt(rwc io.ReadWriteCloser, localPath, _ string, readonly bool, names []string) (*sftp.RequestServer, *rootedHandlers, error) {
	return newRootedServer(rwc, localPath, readonly, names)
}

func (*rootedHandlers) Close() error { return nil }

func (*rootedHandlers) expectRemove(string) bool { return false }

func ServeReadOnlyProject(rwc io.ReadWriteCloser, localPath string, _ bool, _ ProjectMountProbe) error {
	_, _, err := newRootedServer(rwc, localPath, true, nil)
	return err
}
