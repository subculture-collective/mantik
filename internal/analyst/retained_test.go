package analyst

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"git.subcult.tv/subculture-collective/mantik/internal/strategy"
	compiled "git.subcult.tv/subculture-collective/mantik/internal/strategy/analyst"
)

func TestRetainedAnalystOperationalClocksAndHolds(t *testing.T) {
	for _, mode := range []string{"target", "outage", "interrupted", "synthetic"} {
		t.Run(mode, func(t *testing.T) {
			r, q, _, journal, calls := fixture(t, "1")
			if mode != "synthetic" {
				q.Data.EvidenceClass = "prospective_market_unqualified"
			}
			if mode == "outage" {
				calls.fail = errors.New("fixture outage")
				calls.failAt = 1
			}
			original, err := r.Run(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "interrupted" {
				events := journal.events["analyst-"+q.ID]
				journal.events["analyst-"+q.ID] = events[:len(events)-1]
			}
			request, result, err := Retained(context.Background(), journal, q.ID)
			if err != nil {
				t.Fatal(err)
			}
			if result.Receipt.CycleID != q.ID || result.Receipt.ForwardDeclarationID != q.ForwardDeclarationID || result.Receipt.RequestSHA256 != original.RequestSHA256 || len(result.Receipt.ResultSHA256) != 64 {
				t.Fatal("retained identity missing", result)
			}
			adapter, err := compiled.New(request.Config, result.Receipt)
			if err != nil {
				t.Fatal(err)
			}
			spec, _ := request.Config.Spec()
			at := result.Receipt.At
			v, err := strategy.Build(context.Background(), spec, request.Data, at.Add(time.Second), request.Config.Assets, strategy.Book{})
			if err != nil {
				t.Fatal(err)
			}
			d, hold, err := adapter.IncubationDecision(v)
			if mode == "synthetic" {
				if err == nil {
					t.Fatal("synthetic receipt entered operational bridge")
				}
				return
			}
			if err != nil || len(d.State) == 0 {
				t.Fatal(d, hold, err)
			}
			if mode == "target" {
				if hold || len(d.Targets) != 1 || d.Targets[0].Weight != "1" {
					t.Fatal(d, hold)
				}
			} else if !hold || len(d.Targets) != 0 || d.KeepPositions {
				t.Fatal("failure became a resize or flat target", d, hold)
			}
			before, err := strategy.Build(context.Background(), spec, request.Data, at.Add(-time.Nanosecond), request.Config.Assets, strategy.Book{})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = adapter.IncubationDecision(before); err == nil {
				t.Fatal("receipt backdated before completion")
			}
			late, err := strategy.Build(context.Background(), spec, request.Data, at.Add(time.Minute+time.Nanosecond), request.Config.Assets, strategy.Book{})
			if err != nil {
				t.Fatal(err)
			}
			if d, hold, err = adapter.IncubationDecision(late); err != nil || !hold || len(d.Targets) != 0 {
				t.Fatal("expired receipt renewed a target", d, hold, err)
			}
			if len(calls.calls) != original.Calls {
				t.Fatal("retained read relaunched model")
			}
		})
	}
}

func TestRetainedAnalystRejectsChangedTerminalEvidence(t *testing.T) {
	for _, mode := range []string{"request", "strategy", "target", "source", "clock"} {
		t.Run(mode, func(t *testing.T) {
			r, q, _, journal, _ := fixture(t, "1")
			result, err := r.Run(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "request":
				result.RequestSHA256 = "changed"
			case "strategy":
				result.StrategyID = "changed"
			case "target":
				result.Target.Targets[0].Weight = "0.5"
			case "source":
				result.EvidenceClass = "prospective_market_unqualified"
			case "clock":
				result.Receipt.At = result.Receipt.At.Add(time.Second)
			}
			events := journal.events["analyst-"+q.ID]
			events[len(events)-1].Body, _ = json.Marshal(result)
			journal.events["analyst-"+q.ID] = events
			if _, _, err = Retained(context.Background(), journal, q.ID); err == nil {
				t.Fatal("changed terminal evidence admitted", mode)
			}
		})
	}
}
