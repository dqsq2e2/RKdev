package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
)

const networkDefaultsPath = "/etc/rkdev-network.json"
const networkTrialTime = 90 * time.Second

type networkPort struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Path     string `json:"device_path"`
	Wireless bool   `json:"wireless,omitempty"`
}

type wanConfig struct {
	Port    string   `json:"port"`
	Mode    string   `json:"mode"`
	Address string   `json:"address"`
	Gateway string   `json:"gateway"`
	DNS     []string `json:"dns"`
}

type lanConfig struct {
	Ports   []string `json:"ports"`
	Address string   `json:"address"`
	DHCP    bool     `json:"dhcp"`
	Start   string   `json:"start"`
	End     string   `json:"end"`
	NAT     bool     `json:"nat"`
}

type wifiConfig struct {
	Mode     string  `json:"mode"`
	SSID     string  `json:"ssid"`
	Band     string  `json:"band"`
	Channel  int     `json:"channel"`
	Password *string `json:"password,omitempty"`
}

type networkConfig struct {
	WAN  wanConfig  `json:"wan"`
	LAN  lanConfig  `json:"lan"`
	WiFi wifiConfig `json:"wifi"`
}

type networkDefaults struct {
	Ports    []networkPort `json:"ports"`
	Config   networkConfig `json:"defaults"`
	Station  bool          `json:"station"`
	APBands  []string      `json:"ap_bands"`
	Firewall string        `json:"iptables"`
	Store    int64         `json:"store_offset"`
}

func ipv4HostPrefix(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err == nil && p.Addr().Is4() && p.Addr().IsGlobalUnicast() && p.Bits() >= 8 && p.Bits() <= 30 &&
		p.Addr() != p.Masked().Addr() && p.Addr() != lastIPv4(p) {
		return p, nil
	}
	return netip.Prefix{}, fmt.Errorf("请输入有效的 IPv4 地址和前缀，例如 192.168.50.1/24")
}

func lastIPv4(p netip.Prefix) netip.Addr {
	addr := p.Masked().Addr().As4()
	for bit := p.Bits(); bit < 32; bit++ {
		addr[bit/8] |= 1 << (7 - bit%8)
	}
	return netip.AddrFrom4(addr)
}

func (d networkDefaults) validate(c networkConfig) error {
	lan, err := ipv4HostPrefix(c.LAN.Address)
	if err != nil {
		return fmt.Errorf("LAN: %w", err)
	}
	if c.LAN.DHCP {
		start, e1 := netip.ParseAddr(c.LAN.Start)
		end, e2 := netip.ParseAddr(c.LAN.End)
		if e1 != nil || e2 != nil || !lan.Contains(start) || !lan.Contains(end) ||
			start.Compare(end) > 0 || start == lan.Masked().Addr() || end == lastIPv4(lan) ||
			(start.Compare(lan.Addr()) <= 0 && end.Compare(lan.Addr()) >= 0) {
			return fmt.Errorf("DHCP 地址池须位于 LAN 子网内且不能包含管理地址")
		}
	}
	wired := map[string]bool{}
	radio := false
	for _, p := range d.Ports {
		if p.Wireless {
			radio = true
		} else {
			wired[p.ID] = true
		}
	}
	used := map[string]bool{}
	for _, id := range c.LAN.Ports {
		if !wired[id] || used[id] {
			return fmt.Errorf("LAN 网口不存在或重复")
		}
		used[id] = true
	}
	if c.WAN.Mode != "off" && (!wired[c.WAN.Port] && !(radio && d.Station && c.WAN.Port == "wifi")) {
		return fmt.Errorf("请选择有效的 WAN 网口")
	}
	if c.WAN.Mode != "off" && used[c.WAN.Port] {
		return fmt.Errorf("一个物理网口不能同时归属 WAN 和 LAN")
	}
	switch c.WAN.Mode {
	case "static":
		wan, err := ipv4HostPrefix(c.WAN.Address)
		if err != nil {
			return fmt.Errorf("WAN: %w", err)
		}
		if wan.Overlaps(lan) {
			return fmt.Errorf("WAN 和 LAN 网段重叠")
		}
		gw, err := netip.ParseAddr(c.WAN.Gateway)
		if err != nil || !wan.Contains(gw) || gw == wan.Addr() || gw == wan.Masked().Addr() || gw == lastIPv4(wan) {
			return fmt.Errorf("WAN 网关须是同一子网中的其他主机地址")
		}
	case "dhcp", "off":
	default:
		return fmt.Errorf("WAN 模式无效")
	}
	if len(c.WAN.DNS) > 3 {
		return fmt.Errorf("最多填写三个 DNS 地址")
	}
	for _, s := range c.WAN.DNS {
		if ip, err := netip.ParseAddr(s); err != nil || !ip.IsGlobalUnicast() || ip.Zone() != "" {
			return fmt.Errorf("DNS 地址无效")
		}
	}
	if c.WiFi.Mode != "off" {
		if !radio || c.WiFi.SSID == "" || len(c.WiFi.SSID) > 32 ||
			strings.IndexFunc(c.WiFi.SSID, unicode.IsControl) >= 0 {
			return fmt.Errorf("Wi-Fi SSID 无效：长度应为 1–32 字节")
		}
		if c.WiFi.Password != nil && *c.WiFi.Password != "" {
			pw := *c.WiFi.Password
			if len(pw) < 8 || len(pw) > 63 || strings.IndexFunc(pw, func(r rune) bool { return r < 32 || r > 126 }) >= 0 {
				return fmt.Errorf("Wi-Fi 密码请输入 8–63 个可打印 ASCII 字符，或留空使用开放网络")
			}
		}
	}
	switch c.WiFi.Mode {
	case "off":
		if c.WAN.Mode != "off" && c.WAN.Port == "wifi" {
			return fmt.Errorf("Wi-Fi 上联需要启用无线客户端模式")
		}
	case "ap":
		if c.WAN.Mode != "off" && c.WAN.Port == "wifi" {
			return fmt.Errorf("这块板一次只能使用恢复热点或无线客户端")
		}
		if !slices.Contains(d.APBands, c.WiFi.Band) ||
			(c.WiFi.Band == "2.4" && (c.WiFi.Channel < 1 || c.WiFi.Channel > 13)) ||
			(c.WiFi.Band == "5" && !slices.Contains([]int{36, 40, 44, 48, 149, 153, 157, 161}, c.WiFi.Channel)) {
			return fmt.Errorf("请选择板包支持的 Wi-Fi 频段和信道")
		}
	case "station":
		if !d.Station || c.WAN.Port != "wifi" || c.WAN.Mode == "off" {
			return fmt.Errorf("此板没有可用的 Wi-Fi WAN，或尚未选为 WAN")
		}
	default:
		return fmt.Errorf("Wi-Fi 模式无效")
	}
	return nil
}

type networkTrial struct {
	ID       string    `json:"id"`
	Status   string    `json:"status"`
	LANURL   string    `json:"lan_url"`
	Deadline time.Time `json:"deadline"`
	config   networkConfig
}

type networkTrialView struct {
	*networkTrial
	Remaining int `json:"remaining_seconds"`
}

func trialSeconds(deadline time.Time) int {
	return int(max(0, (time.Until(deadline)+time.Second-1)/time.Second))
}

func viewNetworkTrial(t *networkTrial) *networkTrialView {
	if t == nil {
		return nil
	}
	return &networkTrialView{t, trialSeconds(t.Deadline)}
}

type netController struct {
	mu       sync.Mutex
	defaults networkDefaults
	active   networkConfig
	runtime  *netRuntime
	trial    *networkTrial
	store    *recoverySettingsStore
	message  string
	live     bool
}

var netConfigController *netController
var configTrialMu sync.Mutex

func readNetworkDefaults(path string) (networkDefaults, error) {
	var d networkDefaults
	raw, err := os.ReadFile(path)
	if err != nil {
		return d, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&d); err != nil {
		return d, err
	}
	ids := map[string]bool{}
	wifi := 0
	for _, p := range d.Ports {
		if p.ID == "" || p.Label == "" || p.Path == "" || ids[p.ID] ||
			p.Wireless != (p.ID == "wifi") || strings.ContainsAny(p.Path, "\r\n") {
			return d, fmt.Errorf("板包网络端口定义无效")
		}
		ids[p.ID] = true
		if p.Wireless {
			wifi++
		}
	}
	if wifi > 1 || (d.Firewall != "iptables" && d.Firewall != "iptables-legacy") {
		return d, fmt.Errorf("板包网络能力定义无效")
	}
	return d, d.validate(d.Config)
}

func startNetworkController() *netController {
	d, err := readNetworkDefaults(networkDefaultsPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("network defaults: %v", err)
			startLegacyNetwork()
		}
		return nil
	}
	n := &netController{defaults: d, active: d.Config, runtime: &netRuntime{defs: d}}
	if d.Store != 0 && identityError == nil {
		n.store, err = newRecoverySettingsStore(currentLayout, d.Store)
		if err != nil {
			log.Printf("network settings store: %v", err)
		} else if previous, err := n.store.read(); err == nil && previous.Network != nil && d.validate(*previous.Network) == nil {
			n.active = *previous.Network
		}
	}
	go n.start()
	return n
}

func (n *netController) start() {
	// Allow onboard modules to register, without blocking RKdev or the console.
	n.runtime.waitForPorts()
	c := n.active
	msg, err := n.runtime.apply(c)
	if err != nil {
		log.Printf("network startup: %v", err)
		c = n.defaults.Config
		msg, err = n.runtime.apply(c)
		if err != nil {
			n.runtime.stop()
			startLegacyNetwork()
			msg = "网络管理启动失败，已交回板包默认启动脚本"
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.active, n.message, n.live = c, msg, err == nil
}

func (n *netController) startTrial(c networkConfig, now time.Time) (networkTrial, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.live || n.trial != nil {
		return networkTrial{}, fmt.Errorf("网络尚未就绪或已有一项配置正在试用")
	}
	if c.WiFi.Password == nil {
		c.WiFi.Password = n.active.WiFi.Password
	}
	if err := n.defaults.validate(c); err != nil {
		return networkTrial{}, err
	}
	if err := checkDynamicWANOverlap(c, n.runtime.portNames()); err != nil {
		// Only use an existing DHCP lease as a preflight if WAN stays on the same port.
		if n.active.WAN.Mode == c.WAN.Mode && n.active.WAN.Port == c.WAN.Port {
			return networkTrial{}, err
		}
	}
	if !configTrialMu.TryLock() {
		return networkTrial{}, fmt.Errorf("已有网络、防火墙或 SSH 配置正在应用")
	}
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		configTrialMu.Unlock()
		return networkTrial{}, err
	}
	ip, _ := netip.ParsePrefix(c.LAN.Address)
	t := &networkTrial{ID: fmt.Sprintf("%x", id), Status: "applying", LANURL: "http://" + ip.Addr().String() + "/",
		Deadline: now.Add(networkTrialTime), config: c}
	n.trial = t
	snapshot := *t
	go n.applyTrial(t)
	return snapshot, nil
}

func (n *netController) applyTrial(t *networkTrial) {
	time.Sleep(450 * time.Millisecond) // respond before reconfiguring the current link
	msg, err := n.runtime.apply(t.config)
	n.mu.Lock()
	if err != nil {
		t.Status = "reverting"
		n.mu.Unlock()
		n.restore(t, "应用失败，已恢复先前网络："+err.Error())
		return
	}
	t.Status, n.message = "ready", msg
	n.mu.Unlock()
	time.AfterFunc(time.Until(t.Deadline), func() {
		n.mu.Lock()
		if n.trial != t || t.Status != "ready" {
			n.mu.Unlock()
			return
		}
		t.Status = "reverting"
		n.mu.Unlock()
		n.restore(t, "试用超时，已恢复先前网络")
	})
}

func (n *netController) restore(t *networkTrial, message string) {
	n.mu.Lock()
	c := n.active
	n.mu.Unlock()
	_, err := n.runtime.apply(c)
	if err != nil {
		n.runtime.stop()
		startLegacyNetwork()
		message = "回退失败，已交回板包默认启动脚本"
	}
	n.mu.Lock()
	n.trial, n.message, n.live = nil, message, err == nil
	n.mu.Unlock()
	configTrialMu.Unlock()
}

func (n *netController) finish(id, action string, now time.Time) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	t := n.trial
	if t == nil || t.ID != id || t.Status != "ready" || !now.Before(t.Deadline) {
		return "", fmt.Errorf("试用已过期、正在应用或已结束")
	}
	if action == "revert" {
		t.Status = "reverting"
		go n.restore(t, "已恢复先前网络")
		return "正在恢复先前网络", nil
	}
	if warning := wanLeaseWarning(); warning != "" {
		return "", fmt.Errorf("%s", warning)
	}
	if err := checkDynamicWANOverlap(t.config, n.runtime.portNames()); err != nil {
		return "", err
	}
	message := "已生效，仅本次启动"
	if n.store != nil {
		if err := n.store.save(func(s *recoverySettings) { s.Network = &t.config }); err != nil {
			return "", fmt.Errorf("未能保存，试用仍会自动回退: %w", err)
		}
		message = "已保存并生效"
	}
	n.active, n.trial, n.message = t.config, nil, message
	configTrialMu.Unlock()
	return message, nil
}

func handleNetworkConfig(w http.ResponseWriter, r *http.Request) {
	n := netConfigController
	if r.Method == http.MethodGet {
		if n == nil {
			jsonOK(w, map[string]any{"supported": false, "message": "板包尚未提供可配置的网口定义"})
			return
		}
		n.mu.Lock()
		defer n.mu.Unlock()
		c := n.active
		if n.trial != nil {
			c = n.trial.config
		}
		secured := c.WiFi.Password != nil && *c.WiFi.Password != ""
		c.WiFi.Password = nil
		ports := n.runtime.portNames()
		ips := map[string]string{}
		for id, name := range ports {
			ips[id] = runtimeIPv4(name)
		}
		jsonOK(w, map[string]any{"supported": true, "ready": n.live, "config": c, "ports": n.defaults.Ports,
			"names": ports, "addresses": ips, "lan_actual": runtimeIPv4("br0"), "secured": secured,
			"station": n.defaults.Station, "ap_bands": n.defaults.APBands, "persistent": n.store != nil,
			"trial": viewNetworkTrial(n.trial), "message": n.message, "warning": wanLeaseWarning()})
		return
	}
	if r.Method != http.MethodPost {
		jsonErr(w, 405, "仅支持 GET/POST")
		return
	}
	if n == nil {
		jsonErr(w, 409, "板包尚未提供可配置的网口定义")
		return
	}
	var req struct {
		Action string         `json:"action"`
		ID     string         `json:"id"`
		Config *networkConfig `json:"config"`
	}
	if err := decodeSettingsRequest(w, r, &req, 8192); err != nil {
		jsonErr(w, 400, "网络设置请求无效")
		return
	}
	switch req.Action {
	case "apply", "defaults":
		if req.Action == "defaults" {
			c := n.defaults.Config
			empty := ""
			if c.WiFi.Password == nil {
				c.WiFi.Password = &empty
			}
			req.Config = &c
		}
		if req.Config == nil {
			jsonErr(w, 400, "缺少网络配置")
			return
		}
		t, err := n.startTrial(*req.Config, time.Now())
		if err != nil {
			jsonErr(w, 409, err.Error())
		} else {
			jsonOK(w, viewNetworkTrial(&t))
		}
	case "confirm", "revert":
		msg, err := n.finish(req.ID, req.Action, time.Now())
		if err != nil {
			jsonErr(w, 409, err.Error())
		} else {
			jsonOK(w, map[string]string{"message": msg})
		}
	default:
		jsonErr(w, 400, "未知的网络设置操作")
	}
}

func wanLeaseWarning() string {
	warning, _ := os.ReadFile(netRunDir + "/wan-warning")
	return string(warning)
}

func runtimeIPv4(dev string) string {
	iface, err := net.InterfaceByName(dev)
	if err != nil {
		return ""
	}
	addrs, _ := iface.Addrs()
	for _, addr := range addrs {
		if p, err := netip.ParsePrefix(addr.String()); err == nil && p.Addr().Is4() {
			return p.String()
		}
	}
	return ""
}

func checkDynamicWANOverlap(c networkConfig, ports map[string]string) error {
	if c.WAN.Mode == "dhcp" {
		wan, e1 := netip.ParsePrefix(runtimeIPv4(ports[c.WAN.Port]))
		lan, e2 := netip.ParsePrefix(c.LAN.Address)
		if e1 == nil && e2 == nil && wan.Overlaps(lan) {
			return fmt.Errorf("当前 WAN DHCP 租约与 LAN 网段重叠，请改用不同的 LAN 网段")
		}
	}
	return nil
}
