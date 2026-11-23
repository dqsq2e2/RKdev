package main

import (
	"context"
	"crypto/pbkdf2"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const netRunDir = "/run/rkdev-net"

var ifaceName = regexp.MustCompile("^[A-Za-z0-9_.-]{1,15}$")

type networkProcess struct {
	cmd  *exec.Cmd
	done <-chan error
}

type netRuntime struct {
	defs  networkDefaults
	procs []*networkProcess
	wan   string
	lan   []string
}

func (n *netRuntime) portNames() map[string]string {
	names := map[string]string{}
	nodes, _ := os.ReadDir("/sys/class/net")
	for _, p := range n.defs.Ports {
		for _, node := range nodes {
			name := node.Name()
			if !ifaceName.MatchString(name) {
				continue
			}
			device, err := filepath.EvalSymlinks(filepath.Join("/sys/class/net", name, "device"))
			if err != nil || !strings.Contains(device+"/", p.Path+"/") {
				continue
			}
			kind, valid := hardwareInterfaceType("/sys/class/net", name)
			if valid && (kind == "无线") == p.Wireless {
				if names[p.ID] != "" {
					delete(names, p.ID) // Never guess between two matching connectors.
					break
				}
				names[p.ID] = name
			}
		}
	}
	return names
}

func (n *netRuntime) waitForPorts() {
	for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if len(n.portNames()) == len(n.defs.Ports) {
			return
		}
	}
}

func startLegacyNetwork() {
	if _, err := os.Stat("/etc/rkdev-network"); err != nil {
		return
	}
	go func() {
		cmd := exec.Command("/etc/rkdev-network")
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			log.Printf("legacy board network: %v", err)
		}
	}()
}

func netExec(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2800*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(out))
		if len(text) > 200 {
			text = text[:200]
		}
		return fmt.Errorf("%s: %v %s", name, err, text)
	}
	return nil
}

func (n *netRuntime) spawn(name string, env []string, args ...string) error {
	f, err := os.OpenFile(filepath.Join(netRunDir, filepath.Base(name)+".log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	select {
	case <-time.After(180 * time.Millisecond):
		n.procs = append(n.procs, &networkProcess{cmd, done})
		return nil
	case err := <-done:
		return fmt.Errorf("%s 未能启动: %v", name, err)
	}
}

func (n *netRuntime) stop() {
	for i := len(n.procs) - 1; i >= 0; i-- {
		p := n.procs[i]
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(450 * time.Millisecond):
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	}
	n.procs = nil
	if n.wan != "" {
		_ = netExec("ip", "-4", "route", "flush", "dev", n.wan, "scope", "global")
		_ = netExec("ip", "-4", "addr", "flush", "dev", n.wan)
	}
	for _, name := range n.lan {
		_ = netExec("ip", "link", "set", "dev", name, "nomaster")
	}
	n.wan, n.lan = "", nil
	_ = netExec("ip", "-4", "addr", "flush", "dev", "br0")
	_ = netExec(n.defs.Firewall, "-w", "2", "-t", "nat", "-F", "RKDEV_WAN_NAT")
	_ = os.Remove(netRunDir + "/wan-warning")
}

func (n *netRuntime) apply(c networkConfig) (string, error) {
	if err := os.MkdirAll(netRunDir, 0700); err != nil {
		return "", err
	}
	names := n.portNames()
	n.stop()
	for _, job := range [][]string{{"link", "set", "dev", "br0", "up"}, {"-4", "addr", "replace", c.LAN.Address, "dev", "br0"}} {
		if _, err := os.Stat("/sys/class/net/br0/bridge"); os.IsNotExist(err) {
			if err := netExec("ip", "link", "add", "name", "br0", "type", "bridge"); err != nil {
				return "", err
			}
		}
		if err := netExec("ip", job...); err != nil {
			return "", err
		}
	}
	var missing []string
	for _, port := range c.LAN.Ports {
		name := names[port]
		if name == "" {
			missing = append(missing, port)
			continue
		}
		n.lan = append(n.lan, name)
		if err := netExec("ip", "-4", "addr", "flush", "dev", name); err != nil {
			return "", err
		}
		if err := netExec("ip", "link", "set", "dev", name, "master", "br0", "up"); err != nil {
			return "", err
		}
	}
	if err := writeResolvers(c.WAN.DNS); err != nil {
		return "", err
	}
	if c.WiFi.Mode != "off" {
		wifi := names["wifi"]
		if wifi == "" {
			missing = append(missing, "Wi-Fi")
		} else if err := n.configureWiFi(wifi, c.WiFi); err != nil {
			return "", err
		}
	}
	n.wan = names[c.WAN.Port]
	if c.WAN.Mode == "off" {
		n.wan = ""
	} else if n.wan == "" {
		missing = append(missing, "WAN")
	} else {
		if err := netExec("ip", "link", "set", "dev", n.wan, "up"); err != nil {
			return "", err
		}
		if c.WAN.Mode == "dhcp" {
			env := []string{"RKDEV_DHCP_CALLBACK=1", "RKDEV_DHCP_DEV=" + n.wan, "RKDEV_DHCP_LAN=" + c.LAN.Address,
				"RKDEV_DHCP_DNS=" + strings.Join(c.WAN.DNS, " ")}
			if err := n.spawn("udhcpc", env, "-f", "-i", n.wan, "-s", "/usr/bin/rkdev"); err != nil {
				return "", err
			}
		} else {
			if err := netExec("ip", "-4", "addr", "replace", c.WAN.Address, "dev", n.wan); err != nil {
				return "", err
			}
			if err := netExec("ip", "-4", "route", "replace", "default", "via", c.WAN.Gateway, "dev", n.wan); err != nil {
				return "", err
			}
		}
	}
	if err := n.configureNAT(c.LAN.NAT); err != nil {
		return "", err
	}
	lan, _ := netip.ParsePrefix(c.LAN.Address)
	dns := "interface=br0\nbind-interfaces\n"
	if c.LAN.DHCP {
		mask := net.IP(net.CIDRMask(lan.Bits(), 32)).String()
		router := ""
		if !c.LAN.NAT || c.WAN.Mode == "off" {
			router = "dhcp-option=option:router\n"
		}
		dns += fmt.Sprintf("dhcp-range=%s,%s,%s,12h\n%s"+
			"dhcp-option=option:dns-server,%s\ndhcp-authoritative\ndhcp-leasefile=%s/leases\n",
			c.LAN.Start, c.LAN.End, mask, router, lan.Addr(), netRunDir)
	}
	if err := os.WriteFile(netRunDir+"/dnsmasq.conf", []byte(dns), 0600); err != nil {
		return "", err
	}
	if err := n.spawn("dnsmasq", nil, "--keep-in-foreground", "--conf-file="+netRunDir+"/dnsmasq.conf"); err != nil {
		return "", err
	}
	if len(missing) != 0 {
		return "未检测到接口：" + strings.Join(missing, "、"), nil
	}
	return "", nil
}

func (n *netRuntime) configureNAT(on bool) error {
	if !on || n.wan == "" {
		return os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("0\n"), 0644)
	}
	fw := func(args ...string) error {
		return netExec(n.defs.Firewall, append([]string{"-w", "2", "-t", "nat"}, args...)...)
	}
	if err := fw("-F", "RKDEV_WAN_NAT"); err != nil {
		if err := fw("-N", "RKDEV_WAN_NAT"); err != nil {
			return err
		}
	}
	if err := fw("-C", "POSTROUTING", "-j", "RKDEV_WAN_NAT"); err != nil {
		if err := fw("-A", "POSTROUTING", "-j", "RKDEV_WAN_NAT"); err != nil {
			return err
		}
	}
	if err := fw("-A", "RKDEV_WAN_NAT", "-o", n.wan, "-j", "MASQUERADE"); err != nil {
		return err
	}
	return os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0644)
}

func (n *netRuntime) configureWiFi(name string, w wifiConfig) error {
	var psk string
	if w.Password != nil && *w.Password != "" {
		key, err := pbkdf2.Key(sha1.New, *w.Password, []byte(w.SSID), 4096, 32)
		if err != nil {
			return err
		}
		psk = hex.EncodeToString(key)
	}
	ssid := hex.EncodeToString([]byte(w.SSID))
	if w.Mode == "station" {
		auth := "key_mgmt=NONE"
		if psk != "" {
			auth = "key_mgmt=WPA-PSK\npsk=" + psk
		}
		conf := fmt.Sprintf("network={\nssid=%s\n%s\n}\n", ssid, auth)
		path := netRunDir + "/wpa_supplicant.conf"
		if err := os.WriteFile(path, []byte(conf), 0600); err != nil {
			return err
		}
		return n.spawn("wpa_supplicant", nil, "-i", name, "-c", path)
	}
	hwmode := "g"
	if w.Band == "5" {
		hwmode = "a"
	}
	auth := "wpa=0\n"
	if psk != "" {
		auth = "wpa=2\nwpa_key_mgmt=WPA-PSK\nrsn_pairwise=CCMP\nwpa_psk=" + psk + "\n"
	}
	conf := fmt.Sprintf("interface=%s\nbridge=br0\ndriver=nl80211\nctrl_interface=/run/hostapd\n"+
		"ssid2=%s\nhw_mode=%s\nchannel=%d\nieee80211n=1\nwmm_enabled=1\nauth_algs=1\n%s",
		name, ssid, hwmode, w.Channel, auth)
	path := netRunDir + "/hostapd.conf"
	if err := os.WriteFile(path, []byte(conf), 0600); err != nil {
		return err
	}
	if err := netExec("ip", "link", "set", "dev", name, "up"); err != nil {
		return err
	}
	return n.spawn("hostapd", nil, path)
}

func writeResolvers(servers []string) error {
	var conf strings.Builder
	for _, s := range servers {
		ip, err := netip.ParseAddr(s)
		if err == nil && ip.IsGlobalUnicast() && ip.Zone() == "" {
			fmt.Fprintf(&conf, "nameserver %s\n", ip)
		}
	}
	return os.WriteFile("/etc/resolv.conf", []byte(conf.String()), 0644)
}

// BusyBox udhcpc invokes this executable instead of an unscoped lease script.
func netDHCPEvent(event string) error {
	dev := os.Getenv("interface")
	if !ifaceName.MatchString(dev) || dev != os.Getenv("RKDEV_DHCP_DEV") || dev == "lo" || dev == "br0" {
		return fmt.Errorf("invalid DHCP callback interface")
	}
	dns := strings.Fields(os.Getenv("RKDEV_DHCP_DNS"))
	flush := func() {
		_ = netExec("ip", "-4", "route", "flush", "dev", dev, "scope", "global")
		_ = netExec("ip", "-4", "addr", "flush", "dev", dev)
	}
	switch event {
	case "deconfig":
		flush()
		return writeResolvers(dns)
	case "renew", "bound":
		ip, err := netip.ParseAddr(os.Getenv("ip"))
		mask := net.ParseIP(os.Getenv("subnet")).To4()
		bits, size := net.IPMask(mask).Size()
		lan, lanErr := netip.ParsePrefix(os.Getenv("RKDEV_DHCP_LAN"))
		if err != nil || !ip.Is4() || !ip.IsGlobalUnicast() || size != 32 || bits == 0 || lanErr != nil {
			return fmt.Errorf("invalid WAN lease")
		}
		p := netip.PrefixFrom(ip, bits)
		if p.Overlaps(lan) {
			flush()
			return os.WriteFile(netRunDir+"/wan-warning", []byte("WAN DHCP 网段与 LAN 重叠，已拒绝该租约"), 0600)
		}
		_ = os.Remove(netRunDir + "/wan-warning")
		if runtimeIPv4(dev) != p.String() {
			flush()
			if err := netExec("ip", "-4", "addr", "replace", p.String(), "dev", dev); err != nil {
				return err
			}
		}
		for _, gate := range strings.Fields(os.Getenv("router")) {
			gw, err := netip.ParseAddr(gate)
			if err == nil && p.Contains(gw) && gw.IsGlobalUnicast() && gw != p.Addr() {
				if err := netExec("ip", "-4", "route", "replace", "default", "via", gw.String(), "dev", dev); err != nil {
					return err
				}
				break
			}
		}
		if len(dns) == 0 {
			dns = strings.Fields(os.Getenv("dns"))
		}
		return writeResolvers(dns[:min(3, len(dns))])
	}
	return nil
}
