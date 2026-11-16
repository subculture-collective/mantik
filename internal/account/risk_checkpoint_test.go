package account

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestRiskCheckpointRecoversFullLegacyTail(t *testing.T) {
	at := time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC)
	legacy, err := NewCappedState(DefaultCappedConfig("legacy", "paper", Cash, at))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 4096 {
		legacy.Events = append(legacy.Events, BookCommand{ID: fmt.Sprint(i), Kind: "risk_observe", At: at.Add(time.Duration(i) * time.Second)})
		legacy.Version++
	}
	snap, err := SnapshotCapped(legacy, nil, at.Add(4096*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	oversized := legacy
	oversized.Events = append(oversized.Events, BookCommand{ID: "4096", Kind: "risk_observe", At: at.Add(4096 * time.Second)})
	oversized.Version++
	if _, err = SnapshotCapped(oversized, nil, at.Add(4096*time.Second)); err == nil {
		t.Fatal("raw journal limit removed")
	}
	before, _ := json.Marshal(legacy)
	next, err := ApplyCappedCommand(legacy, BookCommand{ID: "next", Kind: "risk_observe", At: at.Add(4096 * time.Second)})
	if err != nil || next.Version != 4097 || len(next.Events) != 2 {
		t.Fatal(next.Version, len(next.Events), err)
	}
	after, _ := json.Marshal(legacy)
	if string(before) != string(after) {
		t.Fatal("legacy bytes changed")
	}
	if next.Events[0].RiskCheckpoint.Count != 4096 || !reflect.DeepEqual(next.Events[0].RiskCheckpoint.State, snap.Risk) {
		t.Fatal("legacy Observe result changed")
	}
}

func TestRiskCheckpointLeavesSealedPrefixesAndVersionCounts(t *testing.T) {
	at := time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC)
	state, err := NewCappedState(DefaultCappedConfig("prefix", "paper", Cash, at))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		state, err = ApplyCappedCommand(state, BookCommand{ID: fmt.Sprint(i), Kind: "risk_observe", At: at})
		if err != nil {
			t.Fatal(err)
		}
	}
	state, err = ApplyCappedCommand(state, BookCommand{ID: "halt", Kind: "halt", At: at})
	if err != nil {
		t.Fatal(err)
	}
	sealed := CappedPrefix(state, len(state.Events))
	raw, _ := json.Marshal(sealed)
	for i := range 8 {
		state, err = ApplyCappedCommand(state, BookCommand{ID: fmt.Sprint(i + 10), Kind: "risk_observe", At: at})
		if err != nil {
			t.Fatal(err)
		}
	}
	prefix := CappedPrefix(state, len(sealed.Events))
	after, _ := json.Marshal(prefix)
	if string(raw) != string(after) || prefix.Version != 9 || state.Version != 17 {
		t.Fatal("sealed prefix/hash or logical version changed")
	}
	snap, err := SnapshotCapped(state, nil, at)
	if err != nil || !snap.Halted {
		t.Fatal("halt lost", snap, err)
	}
}
