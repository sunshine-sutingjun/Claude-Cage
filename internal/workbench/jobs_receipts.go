package workbench

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/sylvinus/agent-vm/internal/host"
	"github.com/sylvinus/agent-vm/internal/usererror"
)

const jobReceiptLimit = 64
const jobReceiptFile = "job-receipts.json"
const jobReceiptTextLimit = 128

// Receipt messages come only from finishLocked's fixed outcome labels. Raw
// job errors, output, arguments and workspace names belong to the live job.
type jobReceipt struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	State         string `json:"state"`
	Message       string `json:"message,omitempty"`
	Reason        string `json:"reason,omitempty"`
	SettingsSaved bool   `json:"settings_saved,omitempty"`
}

func (r jobReceipt) job() Job {
	j := Job{ID: r.ID, Kind: r.Kind, Message: r.Message, SettingsSaved: r.SettingsSaved}
	switch r.State {
	case "running":
		j.Running = true
	case "failed":
		j.Error = r.Message
		if r.Reason != "" {
			j.Error = r.Reason
		}
	case "canceled":
		j.Canceled = true
	case "unknown":
		j.OutcomeUnknown = true
		j.Message = "后台在确认作业结果前已中断，请核对环境状态和原操作的实际结果"
		j.Error = j.Message // Older clients must not classify this as success.
	}
	return j
}

// The controller owns the profile lock. Its job manager serializes access to
// this journal under the same lock as job admission and completion.
type jobReceipts struct {
	path    string
	entries []jobReceipt
	loaded  bool
}

func (h *jobReceipts) load() error {
	if h.loaded || h.path == "" {
		return nil
	}
	err := host.CheckPrivateFile(h.path)
	if errors.Is(err, os.ErrNotExist) {
		h.loaded = true
		return nil
	}
	if err != nil {
		return err
	}
	f, err := os.Open(h.path)
	if err != nil {
		return err
	}
	defer f.Close()
	const maxSize = 128 << 10
	b, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return err
	}
	var entries []jobReceipt
	if len(b) > maxSize || json.Unmarshal(b, &entries) != nil || entries == nil || len(entries) > jobReceiptLimit {
		return errors.New("作业回执文件无法读取，请保留文件并检查诊断记录")
	}
	seen := make(map[string]bool, len(entries))
	for i, r := range entries {
		if len(r.ID) != 12 || strings.Trim(r.ID, "0123456789abcdef") != "" || seen[r.ID] || r.Kind == "" {
			return errors.New("作业回执身份无效")
		}
		seen[r.ID] = true
		if utf8.RuneCountInString(r.Message) > jobReceiptTextLimit || utf8.RuneCountInString(r.Reason) > jobReceiptTextLimit {
			return errors.New("作业回执摘要超出长度限制")
		}
		switch r.State {
		case "running":
			entries[i].State = "unknown"
		case "failed":
			if r.Message == "" {
				return errors.New("作业失败回执缺少结果摘要")
			}
		case "succeeded", "canceled", "unknown":
		default:
			return errors.New("作业回执状态无效")
		}
	}
	h.entries, h.loaded = entries, true
	return nil
}

func (h *jobReceipts) index(id string) int {
	return slices.IndexFunc(h.entries, func(r jobReceipt) bool { return r.ID == id })
}

func (h *jobReceipts) save() error {
	if h.path == "" {
		return nil
	}
	if !h.loaded {
		return errors.New("作业回执尚未载入")
	}
	return writePrivate(h.path, append([]jobReceipt{}, h.entries...))
}

func (h *jobReceipts) admit(kind string) (Job, []jobReceipt, error) {
	if err := h.load(); err != nil {
		return Job{}, nil, err
	}
	id := randomToken()[:12]
	for h.index(id) >= 0 {
		id = randomToken()[:12]
	}
	j := Job{ID: id, Kind: kind, Running: true, Message: "正在准备…"}
	// Keep the previous slice intact so a rejected admission can restore it.
	old := h.entries
	h.entries = append(slices.Clone(old[max(0, len(old)-jobReceiptLimit+1):]), jobReceipt{ID: id, Kind: kind, State: "running"})
	if err := h.save(); err != nil {
		h.entries = old
		return Job{}, old, fmt.Errorf("无法保存作业回执：%w", err)
	}
	return j, old, nil
}

// Use only known product and system error categories. The original error may
// include command output, arguments, paths or credential-bearing URLs.
func receiptFailureReason(err error) string {
	var project *projectShareError
	var network networkCheckError
	var execution *classifiedExecutionError
	var reason string
	switch {
	case errors.As(err, &project) && project.Recovery == "project-approval":
		reason = "客机项目文件夹尚未获得授权"
	case errors.As(err, &network):
		reason = "客机联网检查未通过"
	case errors.As(err, &execution):
		reason = execution.reason
	default:
		reason = usererror.Reason(err)
	}
	if utf8.RuneCountInString(reason) > jobReceiptTextLimit {
		return ""
	}
	return reason
}

func (h *jobReceipts) finish(i int, j Job, cause error) error {
	state := "succeeded"
	if j.Canceled {
		state = "canceled"
	} else if j.Error != "" {
		state = "failed"
		h.entries[i].Reason = receiptFailureReason(cause)
	}
	h.entries[i].State = state
	h.entries[i].Message = j.Message
	h.entries[i].SettingsSaved = j.SettingsSaved
	return h.save()
}

func (m *jobManager) byID(id string) (Job, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id != "" && m.state.ID == id {
		return m.state, true, nil
	}
	if err := m.receipts.load(); err != nil {
		return Job{}, false, err
	}
	if i := m.receipts.index(id); i >= 0 {
		return m.receipts.entries[i].job(), true, nil
	}
	return Job{}, false, nil
}
