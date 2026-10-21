package main

import (
	"context"
	"log/slog"
	mrand "math/rand"
	"strings"
	"time"

	"github.com/djwkvnwincbnak78/nexus/internal/billing"
	"github.com/djwkvnwincbnak78/nexus/internal/model"
	"github.com/djwkvnwincbnak78/nexus/internal/router"
)

// maybeShadow runs after the client got its bytes. Own context, own timeout —
// a slow shadow provider must never touch the real request.
func (s *server) maybeShadow(reqID string, userID uint, req *model.ChatRequest,
	decision *router.Decision, resp *model.ChatResponse, rawBody []byte, mainLatency int) {

	if (s.db == nil && s.shadowSink == nil) || decision == nil || req.Stream {
		return
	}
	if !s.settings.Bool("enable_shadow_test", false) {
		return
	}
	shadowModel := strings.TrimSpace(s.settings.Get("shadow_model", ""))
	if shadowModel == "" || shadowModel == req.Model {
		return
	}
	rate := s.settings.Int("shadow_sample_rate", 100)
	if rate <= 0 {
		return
	}
	if rate < 100 && mrand.Intn(100) >= rate {
		return
	}

	cands := s.snapshot().candidates[shadowModel]
	if len(cands) == 0 {
		slog.Warn("shadow_model_not_served", "shadow_model", shadowModel)
		return
	}

	shadowReq := *req
	shadowReq.Model = shadowModel
	shadowReq.Stream = false
	shadowReq.Messages = append([]model.Message(nil), req.Messages...)
	shadowReq.Tools = append([]model.Tool(nil), req.Tools...)

	mainPrompt, mainCompletion := 0, 0
	if resp != nil && resp.Usage != nil {
		mainPrompt = resp.Usage.PromptTokens
		mainCompletion = resp.Usage.CompletionTokens
	}
	mainCost := billing.Cost(decision.Model.PricePer1M, mainPrompt+mainCompletion)
	mainChannel := decision.Channel.ID

	go func() {
		timeout := time.Duration(s.settings.Int("shadow_timeout_seconds", 60)) * time.Second
		if timeout <= 0 {
			timeout = 60 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		startedAt := time.Now()
		shadowDecision, shadowResp, err := s.complete(ctx, &shadowReq, cands)
		latency := elapsedMS(startedAt)

		shadowTokens, promptTokens, completionTokens := 0, 0, 0
		shadowCost := model.NUMERIC(0)
		shadowChannel := uint(0)
		if shadowDecision != nil {
			shadowChannel = shadowDecision.Channel.ID
		}
		if shadowResp != nil && shadowResp.Usage != nil {
			promptTokens = shadowResp.Usage.PromptTokens
			completionTokens = shadowResp.Usage.CompletionTokens
			shadowTokens = promptTokens + completionTokens
		}
		if shadowDecision != nil {
			shadowCost = billing.Cost(shadowDecision.Model.PricePer1M, shadowTokens)
		}

		status, errMsg := "ok", ""
		if err != nil {
			status = "error"
			errMsg = err.Error()
		}

		rec := shadowRecord{
			TraceID: reqID, MainModel: req.Model, ShadowModel: shadowModel,
			MainChannel: mainChannel, ShadowChannel: shadowChannel,
			MainLatency: mainLatency, ShadowLatency: latency,
			MainCost: mainCost, ShadowCost: shadowCost,
			MainTokens: mainPrompt + mainCompletion, ShadowTokens: shadowTokens,
			Status: status, ErrorMessage: errMsg,
		}
		werr := s.persistShadow(rec)
		if werr != nil {
			slog.Warn("shadow_log_write_failed", "err", werr, "request_id", reqID)
		}
		s.metrics.Inc("gateway_shadow_requests_total", map[string]string{"status": status})
		slog.Info("shadow_done", "main", req.Model, "shadow", shadowModel,
			"main_ms", mainLatency, "shadow_ms", latency, "status", status)
	}()
}

func nullIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}

type shadowRecord struct {
	TraceID       string
	MainModel     string
	ShadowModel   string
	MainChannel   uint
	ShadowChannel uint
	MainLatency   int
	ShadowLatency int
	MainCost      model.NUMERIC
	ShadowCost    model.NUMERIC
	MainTokens    int
	ShadowTokens  int
	Status        string
	ErrorMessage  string
}

func (s *server) persistShadow(rec shadowRecord) error {
	if s.shadowSink != nil {
		return s.shadowSink(rec)
	}
	if s.db == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO shadow_logs
		  (trace_id, main_model, shadow_model, main_channel, shadow_channel,
		   main_latency, shadow_latency, main_cost, shadow_cost,
		   main_tokens, shadow_tokens, status, error_message)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		rec.TraceID, rec.MainModel, rec.ShadowModel, rec.MainChannel, rec.ShadowChannel,
		rec.MainLatency, rec.ShadowLatency, rec.MainCost, rec.ShadowCost,
		rec.MainTokens, rec.ShadowTokens, rec.Status, nullIfEmpty(rec.ErrorMessage))
	return err
}
