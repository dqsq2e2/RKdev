package main

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type firewallRule struct {
	ID        int    `json:"id,omitempty"`
	Chain     string `json:"chain"`
	Action    string `json:"action"`
	Protocol  string `json:"protocol"`
	SrcIP     string `json:"srcIP"`
	DstIP     string `json:"dstIP"`
	DstPort   string `json:"dstPort"`
	NatTarget string `json:"natTarget"`
	Comment   string `json:"comment,omitempty"`
}

type firewallSave struct {
	Enabled bool           `json:"enabled"`
	Rules   []firewallRule `json:"rules"`
}

type firewallTrial struct {
	ID       string    `json:"id"`
	Deadline time.Time `json:"deadline"`
	Status   string    `json:"status"`
	next     firewallSave
}

type firewallTrialView struct {
	*firewallTrial
	Remaining int `json:"remaining_seconds"`
}

func viewFirewallTrial(t *firewallTrial) *firewallTrialView {
	if t == nil {
		return nil
	}
	return &firewallTrialView{t, trialSeconds(t.Deadline)}
}

type firewallController struct {
	mu      sync.Mutex
	state   firewallSave
	trial   *firewallTrial
	message string
	backend string
	store   *recoverySettingsStore
}

var recoveryFirewall firewallController

type fwHook struct{ table, parent, chain string }

var fwInput = fwHook{"filter", "INPUT", "RKDEV_INPUT"}
var fwForward = fwHook{"filter", "FORWARD", "RKDEV_FORWARD"}
var fwIngress = fwHook{"nat", "PREROUTING", "RKDEV_NAT"}
var fwEgress = fwHook{"nat", "POSTROUTING", "RKDEV_NAT_OUT"}
var fwComment = regexp.MustCompile(`^[A-Za-z0-9 _,.:/+-]{0,80}$`)
var fwPort = regexp.MustCompile(`^[0-9]{1,5}(:[0-9]{1,5})?$`)

func fwAddress(s string) bool {
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Is4()
	}
	p, err := netip.ParsePrefix(s)
	return err == nil && p.Addr().Is4()
}

func ruleArgs(r firewallRule) (fwHook, []string, error) {
	var hook fwHook
	var args []string
	bad := fmt.Errorf("规则无效：检查类型、协议、IPv4 地址和端口")
	if r.Protocol == "" {
		r.Protocol = "all"
	}
	if !slices.Contains([]string{"all", "tcp", "udp", "icmp"}, r.Protocol) ||
		(r.SrcIP != "" && !fwAddress(r.SrcIP)) || (r.DstIP != "" && !fwAddress(r.DstIP)) || !fwComment.MatchString(r.Comment) {
		return hook, nil, bad
	}
	if r.DstPort != "" {
		if (r.Protocol != "tcp" && r.Protocol != "udp") || !fwPort.MatchString(r.DstPort) {
			return hook, nil, bad
		}
		prev := 0
		for _, s := range strings.Split(r.DstPort, ":") {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 || n > 65535 || n < prev {
				return hook, nil, bad
			}
			prev = n
		}
	}
	target := r.Action
	switch r.Chain {
	case "input", "forward":
		hook = fwInput
		if r.Chain == "forward" {
			hook = fwForward
		}
		if !slices.Contains([]string{"ACCEPT", "DROP", "REJECT"}, target) || r.NatTarget != "" {
			return hook, nil, bad
		}
	case "nat":
		target = strings.ToUpper(target)
		switch target {
		case "DNAT":
			hook = fwIngress
			_, portErr := netip.ParseAddrPort(r.NatTarget)
			to, addrErr := netip.ParseAddr(strings.SplitN(r.NatTarget, ":", 2)[0])
			if r.DstPort == "" || addrErr != nil || !to.Is4() || !to.IsGlobalUnicast() ||
				(strings.Contains(r.NatTarget, ":") && portErr != nil) {
				return hook, nil, bad
			}
		case "MASQUERADE":
			hook = fwEgress
			if r.NatTarget != "" {
				return hook, nil, bad
			}
		default:
			return hook, nil, bad
		}
	default:
		return hook, nil, bad
	}
	for _, arg := range [][2]string{{"-p", r.Protocol}, {"-s", r.SrcIP}, {"-d", r.DstIP}, {"--dport", r.DstPort}} {
		if arg[1] != "" && (arg[0] != "-p" || arg[1] != "all") {
			args = append(args, arg[:]...)
		}
	}
	if r.Comment != "" {
		args = append(args, "-m", "comment", strconv.Quote(r.Comment))
	}
	args = append(args, "-j", target)
	if target == "DNAT" {
		args = append(args, "--to-destination", r.NatTarget)
	}
	return hook, args, nil
}

func fwCommand(name, input string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %v %s", name, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func startFirewall(store *recoverySettingsStore, backend string, saved *firewallSave) {
	f := &recoveryFirewall
	f.backend, f.store = backend, store
	if saved == nil {
		// If the process restarted during a trial, remove any unconfirmed hooks.
		for _, h := range []fwHook{fwInput, fwForward, fwEgress, fwIngress} {
			if _, err := fwCommand(backend, "", "-w", "2", "-t", h.table, "-C", h.parent, "-j", h.chain); err == nil {
				_, _ = fwCommand(backend, "", "-w", "2", "-t", h.table, "-D", h.parent, "-j", h.chain)
			}
		}
	} else {
		if err := f.apply(*saved); err != nil {
			f.message = "未能加载保存的防火墙：" + err.Error()
			_ = f.apply(firewallSave{})
		} else {
			f.state = *saved
		}
	}
}

func (f *firewallController) apply(c firewallSave) error {
	byChain := map[fwHook][][]string{}
	if len(c.Rules) > 48 {
		return fmt.Errorf("最多可保存 48 条防火墙规则")
	}
	for _, r := range c.Rules {
		hook, args, err := ruleArgs(r)
		if err != nil {
			return err
		}
		byChain[hook] = append(byChain[hook], args)
	}
	for _, hooks := range [][]fwHook{{fwInput, fwForward}, {fwIngress, fwEgress}} {
		table := hooks[0].table
		var data strings.Builder
		fmt.Fprintf(&data, "*%s\n", table)
		for _, h := range hooks {
			fmt.Fprintf(&data, ":%s - [0:0]\n", h.chain)
		}
		for _, h := range hooks {
			_, err := fwCommand(f.backend, "", "-w", "2", "-t", table, "-C", h.parent, "-j", h.chain)
			if err != nil && c.Enabled {
				fmt.Fprintf(&data, "-I %s 1 -j %s\n", h.parent, h.chain)
			} else if err == nil && !c.Enabled {
				fmt.Fprintf(&data, "-D %s -j %s\n", h.parent, h.chain)
			}
			if !c.Enabled {
				continue
			}
			if h == fwInput {
				fmt.Fprintln(&data, "-A "+h.chain+" -i lo -j RETURN")
			}
			for _, rule := range byChain[h] {
				fmt.Fprintf(&data, "-A %s %s\n", h.chain, strings.Join(rule, " "))
			}
		}
		fmt.Fprintln(&data, "COMMIT")
		if _, err := fwCommand(f.backend+"-restore", data.String(), "--noflush", "-w", "2"); err != nil {
			return err
		}
	}
	return nil
}

func (f *firewallController) preview(next firewallSave) (*firewallTrial, error) {
	if f.trial != nil || !configTrialMu.TryLock() {
		return nil, fmt.Errorf("已有配置正在试用")
	}
	if len(next.Rules) > 48 {
		configTrialMu.Unlock()
		return nil, fmt.Errorf("最多可保存 48 条防火墙规则")
	}
	for _, r := range next.Rules {
		if _, _, err := ruleArgs(r); err != nil {
			configTrialMu.Unlock()
			return nil, err
		}
	}
	t := &firewallTrial{ID: strconv.FormatInt(time.Now().UnixNano(), 16),
		Deadline: time.Now().Add(networkTrialTime), Status: "applying", next: next}
	f.trial, f.message = t, ""
	time.AfterFunc(350*time.Millisecond, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if err := f.apply(t.next); err != nil {
			_ = f.revert("应用失败，已恢复先前防火墙：" + err.Error())
			return
		}
		t.Status = "ready"
		time.AfterFunc(time.Until(t.Deadline), func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.trial == t {
				_ = f.revert("试用超时，已恢复先前防火墙")
			}
		})
	})
	return t, nil
}

func (f *firewallController) revert(message string) error {
	err := f.apply(f.state)
	if err != nil {
		message = "未能恢复先前防火墙：" + err.Error()
	}
	f.message, f.trial = message, nil
	configTrialMu.Unlock()
	return err
}

func handleFirewallStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, 405, "仅支持 GET")
		return
	}
	f := &recoveryFirewall
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.state
	if f.trial != nil {
		c = f.trial.next
	}
	groups := map[string][]firewallRule{}
	for i, rule := range c.Rules {
		rule.ID = i + 1
		groups[rule.Chain] = append(groups[rule.Chain], rule)
	}
	jsonOK(w, map[string]any{"enabled": c.Enabled, "inputRules": groups["input"], "forwardRules": groups["forward"],
		"natRules": groups["nat"], "trial": viewFirewallTrial(f.trial), "persistent": f.store != nil, "message": f.message})
}

type setupFirewallSnapshot struct {
	Config     firewallSave
	Trial      *firewallTrial
	Persistent bool
	Message    string
}

type setupFirewallEdit struct{ Before, Config firewallSave }

func handleSetupFirewall(w http.ResponseWriter, r *http.Request) {
	f := &recoveryFirewall
	switch r.Method {
	case http.MethodGet:
		f.mu.Lock()
		defer f.mu.Unlock()
		jsonOK(w, setupFirewallSnapshot{f.state, f.trial, f.store != nil, f.message})
	case http.MethodPost:
		var edit setupFirewallEdit
		if err := decodeSettingsRequest(w, r, &edit, 48<<10); err != nil {
			jsonErr(w, http.StatusBadRequest, "无效防火墙请求")
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if edit.Before.Enabled != f.state.Enabled || !slices.Equal(edit.Before.Rules, f.state.Rules) {
			jsonErr(w, http.StatusConflict, "防火墙设置已变化；请放弃此页更改并重新读取")
			return
		}
		for i := range edit.Config.Rules {
			edit.Config.Rules[i].ID = 0
		}
		t, err := f.preview(edit.Config)
		if err != nil {
			jsonErr(w, http.StatusConflict, err.Error())
			return
		}
		jsonOK(w, viewFirewallTrial(t))
	default:
		jsonErr(w, http.StatusMethodNotAllowed, "仅支持 GET/POST")
	}
}

func handleFirewallChange(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			jsonErr(w, 405, "仅支持 POST")
			return
		}
		f := &recoveryFirewall
		f.mu.Lock()
		defer f.mu.Unlock()
		var req struct {
			firewallRule
			Enabled    bool   `json:"enabled"`
			Trial      string `json:"trial"`
			RuleAction string `json:"operation"`
		}
		if err := decodeSettingsRequest(w, r, &req, 4096); err != nil {
			jsonErr(w, 400, "无效防火墙请求")
			return
		}
		if action == "trial" {
			t := f.trial
			if t == nil || t.ID != req.Trial || t.Status != "ready" || !time.Now().Before(t.Deadline) {
				jsonErr(w, 409, "试用已结束")
				return
			}
			message := "规则仅本次启动有效"
			switch req.RuleAction {
			case "revert":
				message = "已恢复先前防火墙"
				if err := f.revert(message); err != nil {
					jsonErr(w, 409, err.Error())
					return
				}
			case "confirm":
				if f.store != nil {
					if err := f.store.save(func(s *recoverySettings) { s.Firewall = &t.next }); err != nil {
						jsonErr(w, 409, err.Error())
						return
					}
					message = "已保存防火墙"
				}
				f.message, f.state, f.trial = message, t.next, nil
				configTrialMu.Unlock()
			default:
				jsonErr(w, 400, "无效操作")
				return
			}
			jsonOK(w, map[string]string{"message": message})
			return
		}
		next := f.state
		next.Rules = slices.Clone(next.Rules)
		switch action {
		case "add":
			req.firewallRule.ID = 0
			next.Rules = append(next.Rules, req.firewallRule)
		case "delete":
			i := req.ID - 1
			if i < 0 || i >= len(next.Rules) || next.Rules[i].Chain != req.Chain {
				jsonErr(w, 409, "规则已变化，请刷新后重试")
				return
			}
			next.Rules = slices.Delete(next.Rules, i, i+1)
		case "toggle":
			next.Enabled = req.Enabled
		}
		t, err := f.preview(next)
		if err != nil {
			jsonErr(w, 409, err.Error())
		} else {
			jsonOK(w, viewFirewallTrial(t))
		}
	}
}
