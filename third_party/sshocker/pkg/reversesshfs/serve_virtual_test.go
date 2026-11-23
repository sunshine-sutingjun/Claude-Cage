package reversesshfs

import (
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

func TestReadOnlyProjectMountProbeKeepsTheSourceUntouched(t *testing.T) {
	for name, virtual := range map[string]bool{"linux-and-macos": false, "windows": true} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			if err := os.WriteFile(filepath.Join(directory, "project.txt"), []byte("source"), 0600); err != nil {
				t.Fatal(err)
			}
			serverSide, clientSide := net.Pipe()
			defer serverSide.Close()
			defer clientSide.Close()
			_ = serverSide.SetDeadline(time.Now().Add(5 * time.Second))
			_ = clientSide.SetDeadline(time.Now().Add(5 * time.Second))
			probe := ProjectMountProbe{Filename: ".cage-mount-probe-test", Challenge: "current mounted project"}
			go func() { _ = ServeReadOnlyProject(serverSide, directory, virtual, probe) }()
			client, err := sftp.NewClientPipe(clientSide, clientSide)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			root, err := client.RealPath(".")
			if err != nil {
				t.Fatal(err)
			}
			for name, expected := range map[string]string{probe.Filename: probe.Challenge, "project.txt": "source"} {
				file, err := client.Open(path.Join(root, name))
				if err != nil {
					t.Fatal(err)
				}
				info, err := file.Stat()
				if err != nil || info.Size() != int64(len(expected)) || !info.Mode().IsRegular() {
					t.Fatalf("guest stat %s: %v %v", name, info, err)
				}
				data, err := io.ReadAll(file)
				file.Close()
				if err != nil || string(data) != expected {
					t.Fatalf("guest read %s: %q %v", name, data, err)
				}
			}
			if file, err := client.Create(path.Join(root, "new.txt")); err == nil {
				file.Close()
				t.Fatal("guest created a file in a read-only project")
			}
			if file, err := client.OpenFile(path.Join(root, "project.txt"), os.O_WRONLY); err == nil {
				file.Close()
				t.Fatal("guest opened a source file for writing")
			}
			if err := client.Remove(path.Join(root, "project.txt")); err == nil {
				t.Fatal("guest deleted a source file")
			}
			listed, err := client.ReadDir(root)
			if err != nil || len(listed) != 1 || listed[0].Name() != "project.txt" {
				t.Fatalf("project listing included probe: %v %v", listed, err)
			}
			hostFiles, err := os.ReadDir(directory)
			if err != nil || len(hostFiles) != 1 || hostFiles[0].Name() != "project.txt" {
				t.Fatalf("mount probe changed project files: %v %v", hostFiles, err)
			}
		})
	}
}

func TestServeConfinedVirtualRootAndRevocation(t *testing.T) {
	directory := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "hello.txt"), []byte("live project"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(directory, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, ".git", "config"), []byte("protected"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	symlink := os.Symlink(outside, filepath.Join(directory, "escape")) == nil
	serverSide, clientSide := net.Pipe()
	_ = serverSide.SetDeadline(time.Now().Add(5 * time.Second))
	_ = clientSide.SetDeadline(time.Now().Add(5 * time.Second))
	done := make(chan struct{})
	go func() { defer close(done); _ = ServeConfinedVirtual(serverSide, directory, false, []string{".git"}) }()
	client, err := sftp.NewClientPipe(clientSide, clientSide)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	defer serverSide.Close()
	actual, err := client.RealPath(".")
	if err != nil || actual != "/" {
		t.Fatalf("virtual root exposed host: %q %v", actual, err)
	}
	f, err := client.Open("/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(f)
	f.Close()
	if err != nil || string(data) != "live project" {
		t.Fatalf("read %q %v", data, err)
	}
	temp, err := client.Create("/atomic.tmp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = temp.Write([]byte("atomic save")); err != nil {
		t.Fatal(err)
	}
	temp.Close()
	if err = client.PosixRename("/atomic.tmp", "/saved.txt"); err != nil {
		t.Fatal(err)
	}
	if data, err = os.ReadFile(filepath.Join(directory, "saved.txt")); err != nil || string(data) != "atomic save" {
		t.Fatalf("atomic save %q %v", data, err)
	}
	if f, err := client.Create("/.git/config"); err == nil {
		f.Close()
		t.Fatal("protected metadata writable")
	}
	if symlink {
		if f, err := client.Open("/escape/secret.txt"); err == nil {
			f.Close()
			t.Fatal("root symlink escaped")
		}
	}
	if f, err := client.Open(filepath.ToSlash(filepath.Join(outside, "secret.txt"))); err == nil {
		f.Close()
		t.Fatal("absolute host path escaped")
	}
	if err = serverSide.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
	if _, err = client.Create("/after-revoke"); err == nil {
		t.Fatal("revoked transport writable")
	}
}
