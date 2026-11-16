package service

import (
	"context"
	"encoding/json"
	"time"

	"git.subcult.tv/subculture-collective/mantik/internal/account"
	"git.subcult.tv/subculture-collective/mantik/internal/forward"
	"git.subcult.tv/subculture-collective/mantik/internal/marketdata"
	"git.subcult.tv/subculture-collective/mantik/internal/strategy"
)

// retainQualificationViews takes source evidence from compiled service views,
// never a caller's supplied decision/result. Original bar clocks remain intact.
func retainQualificationViews(p *account.IncubationPlan, views []strategy.View) error {
	if p.Qualification == nil {
		return nil
	}
	q := *p.Qualification
	q.DecisionSources = nil
	for i, v := range views {
		if v.At().IsZero() {
			raw, e := json.Marshal(struct {
				At          time.Time
				CodeControl account.IncubationDecision
			}{p.DecidedAt, p.Decisions[i]})
			if e != nil {
				return e
			}
			q.DecisionSources = append(q.DecisionSources, raw)
			continue
		}
		bars := map[string][]marketdata.AdjustedBar{}
		for _, asset := range v.Assets() {
			bars[asset] = v.Bars(asset)
		}
		raw, e := json.Marshal(struct {
			At        time.Time
			Assets    []string
			Bars      map[string][]marketdata.AdjustedBar
			Positions []strategy.Position
			Fills     []strategy.Fill
			State     []byte
		}{v.At(), v.Assets(), bars, v.Positions(), v.Fills(), v.State()})
		if e != nil {
			return e
		}
		q.DecisionSources = append(q.DecisionSources, raw)
	}
	p.Qualification = &q
	return nil
}

func (r IncubationRunner) validateQualificationRequest(ctx context.Context, req IncubationRequest) error {
	if req.Qualification == nil {
		return nil
	}
	s, ok := r.Store.(forward.AssessmentStore)
	if !ok {
		return forward.ErrInvalid
	}
	p, e := s.ForwardAssessmentPlan(ctx, req.Qualification.PlanID)
	if e != nil {
		return e
	}
	fs, ok := r.Store.(forward.Store)
	if !ok {
		return forward.ErrInvalid
	}
	w, e := fs.ForwardDeclaration(ctx, p.Plan.DeclarationID)
	if e != nil {
		return e
	}
	d := w.Declaration
	if len(req.Strategies) != 1 || req.Strategies[0].SleeveID != d.SleeveID || req.Strategies[0].GateID != d.GateID || req.DecidedAt.Before(d.WindowStart) || !req.ExecuteAt.Before(d.OutcomeDeadline) || req.Qualification.Benchmark.Validate(req.ExecuteAt) != nil || req.Qualification.Benchmark.StrategyID != d.Benchmark.StrategyID {
		return forward.ErrInvalid
	}
	return nil
}
