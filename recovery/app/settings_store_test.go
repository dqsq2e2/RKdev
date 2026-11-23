package main

import (
	"bytes"
	"testing"
)

func TestRecoverySettingsSurviveInterruptedSlot(t *testing.T) {
	l := runtimeLayout{LayoutID: "i3588-layout"}
	store := recoverySettingsStore{layout: l}
	old := networkConfig{LAN: lanConfig{Address: "192.168.88.1/24"}}
	next := networkConfig{LAN: lanConfig{Address: "192.168.89.1/24"}}
	a, err := encodeSettingsSlot(settingsSlot{1, recoverySettings{Network: &old}}, l.LayoutID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := encodeSettingsSlot(settingsSlot{2, recoverySettings{Network: &next}}, l.LayoutID)
	if err != nil {
		t.Fatal(err)
	}
	all := append(a, b...)
	slot, i, err := store.load(bytes.NewReader(all))
	if err != nil || i != 1 || slot.Value.Network.LAN.Address != next.LAN.Address {
		t.Fatalf("did not read latest settings: %v %d %v", slot, i, err)
	}
	all[settingsSlotSize+settingsHeaderSize+5] ^= 1
	slot, i, err = store.load(bytes.NewReader(all))
	if err != nil || i != 0 || slot.Value.Network.LAN.Address != old.LAN.Address {
		t.Fatalf("did not fall back after corrupted slot: %v %d %v", slot, i, err)
	}
}
