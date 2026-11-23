package reversesshfs

import (
	"io"
	"os"
	"time"
)

// ProjectMountProbe is a file visible only by direct lookup on a read-only
// project transport. It lets the guest verify the mount without touching disk.
type ProjectMountProbe struct {
	Filename  string
	Challenge string
}

type projectProbeFile struct {
	ProjectMountProbe
	created time.Time
}

func (f projectProbeFile) Name() string       { return f.Filename }
func (f projectProbeFile) Size() int64        { return int64(len(f.Challenge)) }
func (f projectProbeFile) Mode() os.FileMode  { return 0444 }
func (f projectProbeFile) ModTime() time.Time { return f.created }
func (f projectProbeFile) IsDir() bool        { return false }
func (f projectProbeFile) Sys() any           { return nil }

// ServeConfined serves only localPath over an already secured transport. It uses
// the same root and metadata protections as builtin reverse-sshfs mounts.
func ServeConfined(transport io.ReadWriteCloser, localPath string, readonly bool, readonlyNames []string) error {
	server, root, err := newRootedServer(transport, localPath, readonly, readonlyNames)
	if err != nil {
		return err
	}
	defer root.Close()
	defer server.Close()
	return server.Serve()
}

// ServeConfinedVirtual exposes the selected directory at / without disclosing
// its host path. All reads, writes and Guard decisions use the same confined
// root and metadata protections as ServeConfined.
func ServeConfinedVirtual(transport io.ReadWriteCloser, localPath string, readonly bool, readonlyNames []string) error {
	server, root, err := newRootedServerAt(transport, localPath, "/", readonly, readonlyNames)
	if err != nil {
		return err
	}
	defer root.Close()
	defer server.Close()
	return server.Serve()
}
