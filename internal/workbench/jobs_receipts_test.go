package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/host"
)

func readAgentJob(t *testing.T, config, id string) Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	data, err := AgentJob(ctx, config, id)
	if err != nil {
		t.Fatal(err)
	}
	var job Job
	if err := json.Unmarshal(data, &job); err != nil || job.ID != id {
		t.Fatalf("unexpected job %s: %v", data, err)
	}
	return job
}

func TestJobReceiptAfterReplacementAndStop(t *testing.T) {
	s := agentTestController(t)
	s.jobs.receipts.path = filepath.Join(s.profile, jobReceiptFile)
	failed, err := s.submitJob("project", func() error {
		s.jobs.execution().settingsSaved()
		_, _ = s.jobs.execution().Write([]byte("private-output-marker\n"))
		return &projectShareError{Path: "/shared/private-workspace", Recovery: "project-approval", Message: "请确认 private-error-marker"}
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitJobIdle(t, &s.jobs)
	if job := readAgentJob(t, s.configPath, failed.ID); job.Workspace == "" || job.Error == "" {
		t.Fatal("live job lost its detailed recovery information")
	}
	started, release := make(chan struct{}), make(chan struct{})
	defer s.jobs.close()
	defer close(release)
	canceled, err := s.submitJob("settings", func() error {
		execution := s.jobs.execution()
		close(started)
		<-execution.ctx.Done()
		execution.settingsSaved()
		<-release
		return execution.ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitJobSignal(t, started)
	job := readAgentJob(t, s.configPath, failed.ID)
	if job.Running || job.Canceled || job.Error == "" || !job.SettingsSaved || job.Workspace != "" || job.Log != "" || strings.Contains(job.Error, "private-") {
		t.Fatalf("failed receipt lost its result or retained private details: %+v", job)
	}
	stop, err := s.submitJob("stop", func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	joined, err := s.submitJob("stop", func() error { t.Error("joined stop executed twice"); return nil })
	if err != nil || joined.ID != stop.ID {
		t.Fatal("duplicate stop was not joined", err)
	}
	if job := readAgentJob(t, s.configPath, canceled.ID); !job.Running {
		t.Fatal("original job was reported terminal before its worker exited")
	}
	// The stop cannot finish until the superseded execution releases its lock.
	release <- struct{}{}
	awaitJobIdle(t, &s.jobs)
	if job := readAgentJob(t, s.configPath, canceled.ID); job.Running || !job.Canceled || job.OutcomeUnknown || !job.SettingsSaved {
		t.Fatalf("original job did not retain its cancellation: %+v", job)
	}
	if job := readAgentJob(t, s.configPath, stop.ID); job.Running || job.Canceled || job.Error != "" {
		t.Fatalf("stop adopted the original job's outcome: %+v", job)
	}
	b, err := os.ReadFile(s.jobs.receipts.path)
	if err != nil || strings.Contains(string(b), "private-") || host.CheckPrivateFile(s.jobs.receipts.path) != nil {
		t.Fatal("receipts must use private storage and exclude job output, error details and workspace paths")
	}
}

// The child exits with an active worker. The next process runs the normal
// controller entry point and reads the original IDs through the agent API.
func TestJobReceiptProcessRestart(t *testing.T) {
	const childProfile = "CAGE_TEST_JOB_RECEIPT_PROFILE"
	if profile := os.Getenv(childProfile); profile != "" {
		var m jobManager
		m.receipts.path = filepath.Join(profile, jobReceiptFile)
		var operation sync.Mutex
		done, err := m.submit(&operation, "setup", 0, nil, nil, func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		awaitJobIdle(t, &m)
		started := make(chan struct{})
		inflight, err := m.submit(&operation, "settings", 0, nil, nil, func() error {
			m.execution().settingsSaved()
			close(started)
			select {}
		})
		if err != nil {
			t.Fatal(err)
		}
		awaitJobSignal(t, started)
		if err := writePrivate(filepath.Join(profile, "test-ids.json"), []string{done.ID, inflight.ID}); err != nil {
			t.Fatal(err)
		}
		os.Exit(23)
	}

	profile := filepath.Join(t.TempDir(), "profile")
	config := filepath.Join(profile, "config.json")
	// This covers two process launches, controller startup, and the normal
	// password derivation for account creation before the first receipt read.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	writer := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestJobReceiptProcessRestart$")
	writer.Env = append(os.Environ(), childProfile+"="+profile)
	out, err := writer.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("interrupted worker fixture: %s, %v", out, err)
	}
	b, err := os.ReadFile(filepath.Join(profile, "test-ids.json"))
	var ids []string
	if err != nil || json.Unmarshal(b, &ids) != nil || len(ids) != 2 {
		t.Fatal("could not read the IDs returned by the first process", err)
	}
	server := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServeShutdownHandoff$")
	server.Env = append(os.Environ(), "CAGE_TEST_SHUTDOWN_CONFIG="+config)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = server.Wait() }()
	for {
		if _, err := AgentCall(ctx, config, "account.status", nil); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("restarted service did not become available")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := AgentCall(ctx, config, "account.create", json.RawMessage(`{"username":"receipt_qa","password":"test-receipt-password","confirm_password":"test-receipt-password"}`)); err != nil {
		t.Fatal(err)
	}
	if job := readAgentJob(t, config, ids[0]); job.Running || job.Canceled || job.OutcomeUnknown || job.Error != "" || job.Kind != "setup" {
		t.Fatalf("confirmed result changed across restart: %+v", job)
	}
	if job := readAgentJob(t, config, ids[1]); job.Running || job.Canceled || !job.OutcomeUnknown || !job.SettingsSaved || job.Error == "" || job.Kind != "settings" {
		t.Fatalf("crashed worker reported a definite result: %+v", job)
	}
	raw, err := AgentCall(ctx, config, "status", nil)
	var status struct {
		Job Job `json:"job"`
	}
	if err != nil || json.Unmarshal(raw, &status) != nil || status.Job.ID != "" {
		t.Fatalf("reading receipts replayed an old operation: %+v, %v", status.Job, err)
	}
}

func TestJobReceiptSafeFailureReasons(t *testing.T) {
	var m jobManager
	var op sync.Mutex
	m.receipts.path = filepath.Join(t.TempDir(), jobReceiptFile)
	const secret = "private-input-token"
	for _, tc := range []struct {
		err  error
		want string
	}{
		{syscall.ENOSPC, "磁盘剩余空间不足"},
		{os.ErrPermission, "系统拒绝访问所需文件或目录"},
		{context.DeadlineExceeded, "等待响应超时"},
		{&projectShareError{Recovery: "project-approval", Path: secret, Message: secret}, "客机项目文件夹尚未获得授权"},
		{networkCheckError{errors.New(secret)}, "客机联网检查未通过"},
		{executionFailure("启动环境", errors.New(secret), "VM files preserved at /private/"+secret+": lima.yaml is missing"), "虚拟机配置缺失，原有磁盘已保留"},
		{errors.New("未知错误：" + secret), "应用设置失败"},
	} {
		job, err := m.submit(&op, "settings", 0, nil, nil, func() error {
			return fmt.Errorf("%s: %w", secret, tc.err)
		})
		if err != nil {
			t.Fatal(err)
		}
		m.workers.Wait()
		var next jobManager
		next.receipts.path = m.receipts.path
		got, found, err := next.byID(job.ID)
		if err != nil || !found || got.Error != tc.want || got.Message != "应用设置失败" || got.Running {
			t.Fatalf("restored %+v, want %q: %v", got, tc.want, err)
		}
	}
	data, err := os.ReadFile(m.receipts.path)
	if err != nil || strings.Contains(string(data), secret) {
		t.Fatal("original error details persisted", err)
	}
}

func TestJobReceiptRetentionAndRejectedAdmission(t *testing.T) {
	var m jobManager
	var op sync.Mutex
	m.receipts.path = filepath.Join(t.TempDir(), jobReceiptFile)
	var first, second string
	for i := 0; i <= jobReceiptLimit; i++ {
		j, err := m.submit(&op, "settings", 0, nil, nil, func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = j.ID
		}
		if i == 1 {
			second = j.ID
		}
		awaitJobIdle(t, &m)
	}
	if _, err := m.submit(&op, "settings", 0, func() error { return context.Canceled }, nil, func() error { t.Error("rejected job ran"); return nil }); err == nil {
		t.Fatal("rejected admission was accepted")
	}
	var fresh jobManager
	fresh.receipts.path = m.receipts.path
	if _, found, err := fresh.byID(first); err != nil || found {
		t.Fatal("oldest receipt survived eviction", err)
	}
	if j, found, err := fresh.byID(second); err != nil || !found || j.Running || j.Error != "" {
		t.Fatal("rejected admission evicted the next receipt", err)
	}
}

func TestJobReceiptWriteFailurePreservesStop(t *testing.T) {
	dir := t.TempDir()
	unavailable := filepath.Join(dir, "file")
	if err := os.WriteFile(unavailable, nil, 0600); err != nil {
		t.Fatal(err)
	}
	var m jobManager
	var op sync.Mutex
	m.receipts.path = filepath.Join(unavailable, jobReceiptFile)
	_, err := m.submit(&op, "settings", 0, func() error {
		t.Error("changed settings before recording admission")
		return nil
	}, nil, func() error { t.Error("ran without receipt"); return nil })
	if err == nil {
		t.Fatal("ordinary job was admitted without a receipt")
	}
	var stops int
	accepted, err := m.submit(&op, "stop", 0, nil, nil, func() error { stops++; return nil })
	if err != nil || !accepted.ReceiptUnavailable {
		t.Fatal("unavailable receipt prevented or hid the stop", err)
	}
	m.workers.Wait()
	if stops != 1 || m.snapshot().Running {
		t.Fatal("stop failed to run once")
	}
}

func TestJobReceiptCheckpointWriteCanRecover(t *testing.T) {
	var m jobManager
	var op sync.Mutex
	dir := t.TempDir()
	profile := filepath.Join(dir, "profile")
	backup := filepath.Join(dir, "saved")
	m.receipts.path = filepath.Join(profile, jobReceiptFile)
	var checkpoint Job
	job, err := m.submit(&op, "settings", 0, nil, nil, func() error {
		if err := os.Rename(profile, backup); err != nil {
			return err
		}
		if err := os.WriteFile(profile, nil, 0600); err != nil {
			return err
		}
		m.execution().settingsSaved()
		checkpoint = m.snapshot()
		if err := os.Remove(profile); err != nil {
			return err
		}
		return os.Rename(backup, profile)
	})
	if err != nil {
		t.Fatal(err)
	}
	m.workers.Wait()
	if !checkpoint.SettingsSaved || !checkpoint.ReceiptUnavailable {
		t.Fatalf("unconfirmed checkpoint was not reported: %+v", checkpoint)
	}
	if live := m.snapshot(); live.ReceiptUnavailable || !live.SettingsSaved || live.Error != "" {
		t.Fatalf("recovered receipt was still marked unavailable: %+v", live)
	}
	var next jobManager
	next.receipts.path = m.receipts.path
	after, found, err := next.byID(job.ID)
	if err != nil || !found || after.Running || !after.SettingsSaved || after.OutcomeUnknown {
		t.Fatalf("terminal checkpoint was not durable: %+v, %v", after, err)
	}
}
