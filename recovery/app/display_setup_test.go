package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

type setupTestTransport func(*http.Request) (*http.Response, error)

func (f setupTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func setupTestResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
}

func setupTestUI(t *testing.T) *setupUI {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return newSetupUI(ctx, nil)
}

func completeSetupUntil(t *testing.T, u *setupUI, ready func() bool) {
	t.Helper()
	for !ready() {
		select {
		case done := <-u.completed:
			done()
		case <-u.ctx.Done():
			t.Fatal("display work did not complete")
		}
	}
}

func TestSetupWaitsForOverview(t *testing.T) {
	u := setupTestUI(t)
	first, release := make(chan struct{}), make(chan struct{})
	n := 0
	u.client.Transport = setupTestTransport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/profile":
			n++
			if n == 1 {
				close(first)
				return setupTestResponse(503, `{"error":"starting"}`), nil
			}
			select {
			case <-release:
				return setupTestResponse(200, `{"board":"Test board"}`), nil
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		case "/api/cpu", "/api/network":
			return setupTestResponse(200, "{}"), nil
		case "/api/emmc":
			return setupTestResponse(500, `{"error":"unavailable"}`), nil
		default:
			return nil, fmt.Errorf("unexpected request %s", r.URL.Path)
		}
	})
	type result struct {
		overview setupOverview
		err      error
	}
	ready := make(chan result, 1)
	go func() {
		v, err := u.awaitOverview()
		ready <- result{v, err}
	}()
	select {
	case <-first:
	case <-u.ctx.Done():
		t.Fatal("overview was not requested")
	}
	select {
	case <-ready:
		t.Fatal("overview returned before recovery was ready")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case v := <-ready:
		if v.err != nil || v.overview.profile.Board != "Test board" || v.overview.emmcErr == nil {
			t.Fatalf("unexpected overview: %+v", v)
		}
	case <-u.ctx.Done():
		t.Fatal("overview was not ready after the service started")
	}
}

func TestSetupNetworkSlowPollStillAllowsKeys(t *testing.T) {
	u := setupTestUI(t)
	started, release := make(chan struct{}), make(chan struct{})
	u.client.Transport = setupTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/network/config" {
			close(started)
			select {
			case <-release:
				return setupTestResponse(200, `{"ready":true,"supported":true,"config":{"wan":{"mode":"off"}}}`), nil
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return setupTestResponse(200, "{}"), nil
	})
	u.enter(setupPageNetwork)
	select {
	case <-started:
	case <-u.ctx.Done():
		t.Fatal("network status was not requested")
	}
	u.updateNetwork()
	u.key("Down")
	if u.selected != 1 {
		t.Fatal("network selection did not move during the status read")
	}
	u.key("Right")
	if u.page != setupPageSSH {
		t.Fatal("could not leave the network page during the status read")
	}
	u.key("Right")
	if u.page != setupPageFlash {
		t.Fatal("could not switch to firmware flash")
	}
	close(release)
	completeSetupUntil(t, u, func() bool {
		return !u.network.io.reading && !u.sshIO.reading && !u.mediaListIO.reading && !u.mediaJobIO.reading
	})
	if u.page != setupPageFlash {
		t.Fatal("an earlier status result changed the current page")
	}
}

func TestSetupBootDraftRanksSurviveReloadAndCanBeDiscarded(t *testing.T) {
	u := setupTestUI(t)
	base := []string{"emmc", "usb", "nvme"}
	u.client.Transport = setupTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Error("editing sent a write request")
			return setupTestResponse(500, "{}"), nil
		}
		if r.URL.Path == "/api/setup/boot" {
			return setupTestResponse(200, `{"Revision":"test", "Order":["emmc","usb","nvme"],"Available":["emmc","usb","nvme","recovery"]}`), nil
		}
		return setupTestResponse(200, "{}"), nil
	})
	u.enter(setupPageBoot)
	completeSetupUntil(t, u, func() bool { return !u.bootIO.reading })
	u.key("Down")
	u.key("+")
	if u.selected != 0 || !slices.Equal(u.boot.edit.Order, []string{"usb", "emmc", "nvme"}) {
		t.Fatalf("plus did not move USB up: %v (selected %d)", u.boot.edit.Order, u.selected)
	}
	u.key("-")
	if u.selected != 1 || !slices.Equal(u.boot.edit.Order, base) {
		t.Fatalf("minus did not move USB back down: %v", u.boot.edit.Order)
	}
	u.key("Down")
	u.key("Enter")
	u.key("Home")
	u.key("Enter")
	want := []string{"nvme", "emmc", "usb"}
	u.key("Right")
	u.key("Left")
	u.key("F5")
	completeSetupUntil(t, u, func() bool { return !u.bootIO.reading && !u.network.io.reading })
	if !slices.Equal(u.boot.edit.Order, want) || !slices.Equal(u.boot.base.Order, base) {
		t.Fatalf("rank dialog or reload lost the draft: %v, saved %v", u.boot.edit.Order, u.boot.base.Order)
	}
	u.key("Escape")
	u.key("End")
	u.key("Enter")
	u.key("Enter")
	if u.page != setupPageExit || len(u.changes()) != 0 || !slices.Equal(u.boot.edit.Order, base) {
		t.Fatal("discard did not restore the saved boot order")
	}
}

func TestSetupSaveAndRevert(t *testing.T) {
	for _, revert := range []bool{false, true} {
		t.Run(fmt.Sprintf("revert=%t", revert), func(t *testing.T) {
			u := setupTestUI(t)
			writes := make(chan string, 8)
			deadline := time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
			var networkReverted bool
			u.client.Transport = setupTestTransport(func(r *http.Request) (*http.Response, error) {
				p := r.URL.Path
				switch {
				case r.Method == http.MethodGet:
					switch p {
					case "/api/network/config":
						if networkReverted {
							return setupTestResponse(200, `{"persistent":true,"message":"restored"}`), nil
						}
						return setupTestResponse(200, fmt.Sprintf(`{"persistent":true,"trial":{"id":"net","status":"ready","deadline":%q}}`, deadline)), nil
					case "/api/setup/firewall":
						return setupTestResponse(200, fmt.Sprintf(`{"persistent":true,"trial":{"id":"fw","status":"ready","deadline":%q}}`, deadline)), nil
					}
				case p == "/api/network/config":
					var req struct{ Action string }
					_ = json.NewDecoder(r.Body).Decode(&req)
					writes <- "net:" + req.Action
					if req.Action == "apply" {
						return setupTestResponse(200, fmt.Sprintf(`{"id":"net","status":"applying","deadline":%q}`, deadline)), nil
					}
					networkReverted = req.Action == "revert"
				case p == "/api/setup/firewall":
					var req setupFirewallEdit
					_ = json.NewDecoder(r.Body).Decode(&req)
					if !req.Config.Enabled || len(req.Config.Rules) != 2 {
						t.Error("firewall draft was not sent as a batch")
					}
					writes <- "firewall:start"
					return setupTestResponse(200, fmt.Sprintf(`{"id":"fw","status":"applying","deadline":%q}`, deadline)), nil
				case p == "/api/firewall/trial":
					writes <- "firewall:confirm"
				case p == "/api/ssh/config":
					writes <- "ssh:save"
				case p == "/api/setup/boot":
					var req setupBootEdit
					_ = json.NewDecoder(r.Body).Decode(&req)
					if !slices.Equal(req.Order, []string{"usb", "emmc"}) || req.Vars["console"] != "serial" {
						t.Error("boot order and environment changes were not sent together")
					}
					writes <- "boot:save"
				default:
					t.Errorf("unexpected request: %s %s", r.Method, p)
				}
				return setupTestResponse(200, `{"message":"ok"}`), nil
			})
			u.page = setupPageExit
			u.network.draft.WAN.Mode = "dhcp"
			u.firewall.draft = firewallSave{true, []firewallRule{{Chain: "input", Action: "ACCEPT"}, {Chain: "forward", Action: "REJECT"}}}
			u.ssh.Persistent = true
			u.sshDraft = sshConfig{Port: 2222, Enabled: true, Password: true}
			u.boot.edit = setupBootEdit{Order: []string{"usb", "emmc"}, Vars: map[string]string{"console": "serial"}}
			u.showExit()
			u.key("F10")
			u.key("Escape")
			u.key("Down")
			u.key("Enter")
			if len(writes) != 0 || u.save != nil {
				t.Fatal("settings were written without accepting the save dialog")
			}
			u.key("Enter")
			completeSetupUntil(t, u, func() bool { return u.save != nil && u.save.trial.Status == "ready" })
			if revert {
				u.key("Escape")
				completeSetupUntil(t, u, func() bool { return !u.save.busy })
				u.update()
				completeSetupUntil(t, u, func() bool { return u.save == nil })
				if len(u.changes()) != 4 || !strings.Contains(u.message, "restored") {
					t.Fatal("revert did not retain all drafts")
				}
			} else {
				u.finishSetupTrial(true)
				completeSetupUntil(t, u, func() bool {
					return u.save != nil && u.save.step == setupSaveFirewall && u.save.trial.Status == "ready"
				})
				u.finishSetupTrial(true)
				completeSetupUntil(t, u, func() bool { return u.save == nil })
				if len(u.changes()) != 0 {
					t.Fatal("save left confirmed values marked as unsaved")
				}
			}
			var got []string
			for len(writes) > 0 {
				got = append(got, <-writes)
			}
			want := []string{"net:apply", "net:revert"}
			if !revert {
				want = []string{"net:apply", "net:confirm", "firewall:start", "firewall:confirm", "ssh:save", "boot:save"}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("write sequence = %v, want %v", got, want)
			}
		})
	}
}

func TestSetupConsoleKeys(t *testing.T) {
	for sequence, want := range map[string]string{
		"\x1b[1~": "Home", "\x1b[4~": "End", "\x1b[21~": "F10",
		"\x1bOk": "+", "\x1bOm": "-", "\x1bOM": "Enter",
	} {
		if got, size := setupKey([]byte(sequence), false); got != want || size != len(sequence) {
			t.Fatalf("setupKey(%q) = %q, %d", sequence, got, size)
		}
	}
}
