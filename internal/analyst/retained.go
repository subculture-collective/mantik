package analyst

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"git.subcult.tv/subculture-collective/mantik/internal/allocation"
	"git.subcult.tv/subculture-collective/mantik/internal/research"
	compiled "git.subcult.tv/subculture-collective/mantik/internal/strategy/analyst"
	"git.subcult.tv/subculture-collective/mantik/internal/strategytrial"
)

// EvidenceReader exposes retained events without model calls or journal writes.
type EvidenceReader interface {
	Events(context.Context, string) ([]research.Event, error)
}

// Retained reads a completed, immutable runtime result without another model
// call. Interrupted sessions produce no target. The source request remains the
// only source of bars, identity and provenance for the execution bridge.
func Retained(ctx context.Context, journal EvidenceReader, id string) (Request, Result, error) {
	bad := errors.New("retained analyst evidence is unavailable or inconsistent")
	if journal == nil || !strategytrial.ValidID(id) || len(id) > 64 {
		return Request{}, Result{}, bad
	}
	events, err := journal.Events(ctx, "analyst-"+id)
	if err != nil {
		return Request{}, Result{}, err
	}
	if len(events) == 0 || events[0].Kind != "start" {
		return Request{}, Result{}, bad
	}
	var request Request
	if json.Unmarshal(events[0].Body, &request) != nil || request.ID != id || request.Validate(request.AsOf) != nil {
		return Request{}, Result{}, bad
	}
	spec, err := request.Config.Spec()
	if err != nil {
		return Request{}, Result{}, err
	}
	sid, _ := spec.ID()
	last := events[len(events)-1]
	if last.At.IsZero() || last.At.Before(events[0].At) {
		return Request{}, Result{}, bad
	}
	calls := 0
	for _, event := range events {
		if event.Kind == "call" {
			calls++
		}
	}
	if calls > len(roles) {
		return Request{}, Result{}, bad
	}
	result := Result{ID: id, StrategyID: sid, RequestSHA256: hash(marshal(request)), EvidenceClass: request.Data.EvidenceClass, SourceSHA256: request.Data.SourceSHA256, Calls: calls, Status: "no_target", Reason: "interrupted_no_relaunch", Target: allocation.TargetDecision{HoldExposure: true}}
	if last.Kind == "done" {
		// Decode into zero values: omitted false fields must not inherit the
		// default exposure hold used only for interrupted sessions.
		var saved Result
		if err := json.Unmarshal(last.Body, &saved); err != nil {
			return Request{}, Result{}, bad
		}
		result = saved
		if json.Unmarshal(last.Body, &result) != nil || result.ID != id || result.StrategyID != sid || result.RequestSHA256 != hash(marshal(request)) || result.EvidenceClass != request.Data.EvidenceClass || result.SourceSHA256 != request.Data.SourceSHA256 || result.QualificationCredit || result.Calls != calls {
			return Request{}, Result{}, bad
		}
		if result.Status != "target" && result.Status != "no_target" {
			return Request{}, Result{}, bad
		}
	}
	receipt := result.Receipt
	if receipt.Completed {
		if receipt.StrategyID != sid || !receipt.InputAt.Equal(request.AsOf) || receipt.At.Before(request.AsOf) || receipt.At.Truncate(time.Microsecond).After(last.At) || receipt.EvidenceClass != result.EvidenceClass || receipt.SourceSHA256 != result.SourceSHA256 {
			return Request{}, Result{}, bad
		}
		v, err := view(ctx, request)
		if err != nil {
			return Request{}, Result{}, err
		}
		_, viewHash := compiled.Input(v)
		if receipt.ViewSHA256 != viewHash {
			return Request{}, Result{}, bad
		}
		if _, err := compiled.New(request.Config, receipt); err != nil {
			return Request{}, Result{}, err
		}
	} else {
		if result.Status != "no_target" {
			return Request{}, Result{}, bad
		}
		receipt = compiled.Receipt{At: last.At, InputAt: request.AsOf, StrategyID: sid, EvidenceClass: result.EvidenceClass, SourceSHA256: result.SourceSHA256}
	}
	expected := allocation.TargetDecision{HoldExposure: true}
	if receipt.Completed && receipt.Selection.Action != "hold" {
		expected = allocation.TargetDecision{}
		if receipt.Selection.Action == "long" {
			expected.Targets = []allocation.WeightTarget{{Asset: receipt.Selection.Asset, Weight: "1"}}
		}
		if result.Status != "target" {
			return Request{}, Result{}, bad
		}
	} else if result.Status != "no_target" {
		return Request{}, Result{}, bad
	}
	if string(marshal(result.Target)) != string(marshal(expected)) {
		return Request{}, Result{}, bad
	}
	receipt.CycleID, receipt.RequestSHA256, receipt.ResultSHA256 = id, result.RequestSHA256, hash(marshal(result))
	receipt.ForwardDeclarationID = request.ForwardDeclarationID
	result.Receipt = receipt
	return request, result, nil
}
