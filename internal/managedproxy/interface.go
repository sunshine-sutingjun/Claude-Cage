package managedproxy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// TransportInterface selects the socket binding used to reach the first proxy.
func (s Store) TransportInterface(ctx context.Context) (string, error) {
	if !s.FirstHopViaHostTunnel {
		return PhysicalInterface(ctx)
	}
	if runtime.GOOS != "darwin" {
		return "", errors.New("上游隧道绑定仅支持 macOS")
	}
	ip, err := s.tunnelFirstHop()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/sbin/route", "-n", "get", "-inet", ip.String()).Output()
	if err != nil {
		return "", errors.New("无法查询首跳代理的 IPv4 路由，外网保持关闭")
	}
	return tunnelRouteInterface(string(out))
}

func (s Store) tunnelFirstHop() (netip.Addr, error) {
	hops, err := s.Route()
	if err != nil {
		return netip.Addr{}, err
	}
	if len(hops) == 0 {
		return netip.Addr{}, errors.New("缺少上游隧道的首跳代理")
	}
	entry, _ := s.Find(hops[0])
	server, _ := entry.Config["server"].(string)
	ip, err := netip.ParseAddr(server)
	if entry.Config["type"] != "socks5" || err != nil || !publicIP(ip) {
		return netip.Addr{}, errors.New("上游隧道绑定需要首跳使用公网 IPv4 地址的 SOCKS5 节点")
	}
	return ip, nil
}

var macTunnelInterface = regexp.MustCompile(`^utun[0-9]+$`)

func tunnelRouteInterface(route string) (string, error) {
	var name, flags string
	for _, line := range strings.Split(route, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		switch fields[0] {
		case "interface:":
			name = fields[1]
		case "flags:":
			flags = "," + strings.Trim(fields[1], "<>") + ","
		}
	}
	if !macTunnelInterface.MatchString(name) || !strings.Contains(flags, ",UP,") || strings.Contains(flags, ",REJECT,") || strings.Contains(flags, ",BLACKHOLE,") {
		return "", errors.New("首跳代理缺少可用的 macOS 隧道路由，外网保持关闭")
	}
	return name, nil
}

// PhysicalInterface uses the system's preferred reachable IPv4 hardware port.
// A route through utun is not a physical uplink, even when it is the default.
func PhysicalInterface(ctx context.Context) (string, error) {
	if runtime.GOOS == "windows" {
		return windowsPhysicalInterface(ctx)
	}
	if runtime.GOOS == "linux" {
		return linuxPhysicalInterface(ctx)
	}
	if runtime.GOOS != "darwin" {
		return "", errors.New("物理网卡识别当前支持 macOS")
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	hardware, err := exec.CommandContext(ctx, "/usr/sbin/networksetup", "-listallhardwareports").Output()
	if err != nil {
		return "", errors.New("无法读取物理网卡，请检查主机网络后重试")
	}
	route, _ := exec.CommandContext(ctx, "/sbin/route", "-n", "get", "default").Output()
	nwi, _ := exec.CommandContext(ctx, "/usr/sbin/scutil", "--nwi").Output()
	usable := map[string]bool{}
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, _ := iface.Addrs()
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && ip.To4() != nil && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified() {
				usable[iface.Name] = true
			}
		}
	}
	return selectPhysicalInterface(string(route), string(hardware), string(nwi), usable)
}

// macOS also exposes packet tunnels, bridges, VM links, AWDL and other
// service interfaces through network tools. They can carry an IPv4 address,
// but they are not a physical uplink and must not become the proxy's binding.
var macVirtualInterface = regexp.MustCompile(`(?i)^(?:utun|bridge|vmen|vmenet|vmnet|tap|lima|awdl|llw|p2p|gif|stf|anpi|ap)[0-9]+$`)

func isVirtualMACInterface(name string) bool {
	return macVirtualInterface.MatchString(name)
}

func selectPhysicalInterface(route, hardware, nwi string, usable map[string]bool) (string, error) {
	physical := map[string]bool{}
	for _, line := range strings.Split(hardware, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "Device:" && usable[fields[1]] && !isVirtualMACInterface(fields[1]) {
			physical[fields[1]] = true
		}
	}
	// scutil lists interfaces in network priority order. Only its reachable
	// IPv4 section is relevant to the proxy's IPv4 infrastructure transport.
	inIPv4, candidate := false, ""
	reachable := []string{}
	for _, line := range strings.Split(nwi, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "IPv4 network interface information") {
			inIPv4 = true
		} else if strings.HasPrefix(line, "IPv6 network interface information") {
			break
		}
		if !inIPv4 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[1] == ":" && fields[2] == "flags" {
			candidate = fields[0]
		}
		if strings.HasPrefix(line, "reach") && strings.Contains(line, "(Reachable)") && physical[candidate] {
			reachable = append(reachable, candidate)
		}
	}
	for _, line := range strings.Split(route, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "interface:" {
			for _, name := range reachable {
				if fields[1] == name {
					return name, nil
				}
			}
		}
	}
	if len(reachable) > 0 {
		return reachable[0], nil
	}
	return "", errors.New("未找到可用的物理 IPv4 网卡；Cage 外网保持关闭，请连接网络后重试")
}

// BindInterface changes only the core's host transport. MATCH routing and
// proxy credentials remain intact; this does not create a guest direct route.
func BindInterface(data []byte, name string) ([]byte, error) {
	if name == "" {
		return nil, errors.New("缺少物理网卡，不能启动代理")
	}
	var config map[string]any
	if err := yaml.Unmarshal(data, &config); err != nil || config == nil {
		return nil, errors.New("代理运行配置无效")
	}
	config["interface-name"] = name
	// Older imports may carry a node-level binding, which overrides the global.
	if nodes, ok := config["proxies"].([]any); ok {
		for _, item := range nodes {
			if node, ok := item.(map[string]any); ok {
				if _, bound := node["interface-name"]; bound {
					node["interface-name"] = name
				}
			}
		}
	}
	return yaml.Marshal(config)
}

func BoundInterface(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var config struct {
		Interface string `yaml:"interface-name"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return "", err
	}
	return config.Interface, nil
}
